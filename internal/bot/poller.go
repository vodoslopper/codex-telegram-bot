package bot

import (
	"context"
	"fmt"
	"math/rand/v2"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"codex-telegram-bot/internal/telegram"
)

// maxPollBackoff caps the delay between failed getUpdates calls.
const maxPollBackoff = 60 * time.Second

// Prepare does the one-off Telegram handshake before polling starts.
//
// It removes any webhook, because a webhook and a poller are mutually exclusive:
// with a webhook configured, getUpdates keeps answering 409 and the bot looks
// alive while receiving nothing. Pending updates are deliberately *not* dropped,
// so messages sent while the bot was down are still delivered.
//
// A network failure here is not fatal — Telegram may simply be unreachable right
// now, and the poller retries. A rejected token is fatal, because retrying it
// will never succeed.
func (b *Bot) Prepare(ctx context.Context) error {
	if b.cfg.DeleteWebhook {
		if info, err := b.tg.GetWebhookInfo(ctx); err != nil {
			b.log.Warn("could not read the webhook configuration", "error", err.Error())
		} else if info.URL != "" {
			b.log.Warn("a webhook is configured for this bot; removing it because this bot polls",
				"webhook_url", info.URL, "pending_updates", info.PendingUpdateCount)
		}
		if err := b.tg.DeleteWebhook(ctx, false); err != nil {
			if telegram.IsUnauthorized(err) {
				return fmt.Errorf("Telegram rejected the bot token: %w", err)
			}
			b.log.Warn("could not delete the webhook; getUpdates may fail with 409 until it is removed",
				"error", err.Error())
		} else {
			b.log.Info("webhook cleared")
		}
	}

	me, err := b.tg.GetMe(ctx)
	if err != nil {
		if telegram.IsUnauthorized(err) {
			return fmt.Errorf("Telegram rejected the bot token: %w", err)
		}
		b.log.Warn("could not reach Telegram yet; starting to poll anyway", "error", err.Error())
		return nil
	}
	// TrimPrefix because a username from getMe has no "@", but be tolerant if
	// that ever changes: ParseCommand compares bare names.
	b.botUsername = strings.TrimPrefix(me.Username, "@")
	b.log.Info("connected to Telegram",
		"bot_id", me.ID, "bot_username", me.Username,
		"allowed_users", len(b.cfg.AllowedUserIDs))
	return nil
}

// Poll runs the getUpdates loop until ctx is cancelled.
//
// The offset is advanced only after every update in a batch has been claimed in
// the database. That ordering, and not the polling itself, is what makes a
// restart safe: a claimed update is never re-run, and an unclaimed one is
// redelivered by Telegram and handled exactly once.
//
// Handling is dispatched to a goroutine so the loop keeps polling while a Codex
// turn runs. If it did not, /stop could never arrive while a turn was in flight.
func (b *Bot) Poll(ctx context.Context) error {
	offset, err := b.st.GetOffset(ctx)
	if err != nil {
		return fmt.Errorf("read the stored update offset: %w", err)
	}
	b.log.Info("polling Telegram for updates",
		"offset", offset, "api_base", b.cfg.TelegramAPIBase,
		"limit", b.cfg.PollLimit, "long_poll", b.cfg.PollTimeout,
		"max_concurrent_turns", b.cfg.MaxConcurrent)

	var backoff time.Duration
	for {
		if err := ctx.Err(); err != nil {
			b.log.Info("stopping the update poller", "reason", err.Error())
			return nil
		}

		updates, err := b.tg.GetUpdates(ctx, offset, b.cfg.PollTimeout, b.cfg.PollLimit)
		if err != nil {
			if telegram.IsUnauthorized(err) {
				return fmt.Errorf("Telegram rejected the bot token: %w", err)
			}
			if ctx.Err() != nil {
				return nil
			}
			backoff = nextBackoff(backoff)
			b.log.Error("getUpdates failed", "error", err.Error(), "retry_in", backoff)
			if !sleepCtx(ctx, backoff) {
				return nil
			}
			continue
		}
		backoff = 0
		if len(updates) == 0 {
			continue
		}

		b.log.Debug("received updates", "count", len(updates), "from_offset", offset)
		next := offset
		for _, u := range updates {
			if ctx.Err() != nil {
				break
			}
			p, cerr := b.Claim(ctx, u)
			if cerr != nil {
				// The claim could not be recorded, so the offset must not move
				// past this update: Telegram will redeliver it and the claim
				// will be retried. Advancing here is how a message would be
				// silently lost.
				b.log.Error("could not claim an update; leaving the offset where it is",
					"update_id", u.UpdateID, "offset", offset, "error", cerr.Error())
				break
			}
			if u.UpdateID+1 > next {
				next = u.UpdateID + 1
			}
			if p != nil {
				b.dispatch(ctx, p)
			}
		}

		if next > offset {
			if aerr := b.st.AdvanceOffset(ctx, next); aerr != nil {
				// Not advancing is safe but wasteful: every update would be
				// redelivered and re-claimed. Loud, not silent.
				b.log.Error("could not persist the update offset",
					"offset", next, "error", aerr.Error())
				continue
			}
			offset = next
		}
	}
}

// dispatch runs one prepared update on its own goroutine.
//
// The handler gets the work context, not the poll context, so stopping the
// poller does not abort a turn that is already running.
//
// A panic in a handler is recovered and logged rather than taking the whole
// service down: one malformed message must not stop the bot for everybody.
func (b *Bot) dispatch(_ context.Context, p *Prepared) {
	ctx := b.workContext()
	turn := !p.Duplicate && !p.Unsupported && p.Cmd == nil
	if turn {
		p.admitted = b.reserveTurn()
		p.busy = !p.admitted
		if p.admitted {
			p.turnCtx, p.cancel = context.WithCancel(ctx)
			p.entry = &inflightTurn{cancel: p.cancel, started: b.now()}
			b.inflight.register(p.Scope, p.entry)
		}
	}
	stop := p.Cmd != nil && (p.Cmd.Name == "stop" || p.Cmd.Name == "cancel")
	var previous <-chan struct{}
	var done chan struct{}
	if !stop && !p.busy {
		b.dispatchMu.Lock()
		previous = b.dispatchTail[p.Scope]
		done = make(chan struct{})
		b.dispatchTail[p.Scope] = done
		b.dispatchMu.Unlock()
	}
	b.wg.Add(1)
	go func() {
		defer b.wg.Done()
		var finishTurn func()
		if p.admitted {
			var once sync.Once
			finishTurn = func() {
				once.Do(func() {
					b.inflight.clear(p.Scope, p.entry)
					p.cancel()
					b.pendingTurns.Add(-1)
				})
			}
			defer finishTurn()
		}
		if done != nil {
			defer func() {
				b.dispatchMu.Lock()
				close(done)
				if b.dispatchTail[p.Scope] == done {
					delete(b.dispatchTail, p.Scope)
				}
				b.dispatchMu.Unlock()
			}()
		}
		defer func() {
			if r := recover(); r != nil {
				b.log.Error("panic while handling an update",
					"update_id", p.UpdateID, "panic", r, "stack", string(debug.Stack()))
			}
		}()
		if previous != nil {
			waitCtx := ctx
			if p.turnCtx != nil {
				waitCtx = p.turnCtx
			}
			queueCtx, cancelQueue := context.WithTimeout(waitCtx, b.cfg.QueueTimeout)
			defer cancelQueue()
			select {
			case <-previous:
			case <-queueCtx.Done():
				if p.admitted {
					b.reportQueueFailure(waitCtx, p, "another turn in this chat", queueCtx.Err())
					finishTurn()
				} else if ctx.Err() == nil {
					b.sendBest(ctx, p.Scope, "This command waited too long for the current turn. Please retry it.")
				}
				// Keep the ordering link until the predecessor ends. Otherwise
				// the following command could overtake the active turn.
				select {
				case <-previous:
				case <-ctx.Done():
				}
				return
			}
		}
		b.Handle(ctx, p)
	}()
}

// nextBackoff doubles a delay from 1s up to maxPollBackoff, with jitter so
// several restarts do not synchronise on Telegram.
func nextBackoff(cur time.Duration) time.Duration {
	if cur <= 0 {
		cur = time.Second
	} else {
		cur *= 2
	}
	if cur > maxPollBackoff {
		cur = maxPollBackoff
	}
	jitter := time.Duration(rand.Int64N(int64(cur)/4 + 1))
	return cur/2 + jitter
}

// sleepCtx waits for d or until ctx ends, reporting whether the full wait
// completed.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// Run prepares the Telegram connection and polls until pollCtx ends.
//
// Two contexts, deliberately:
//
//   - pollCtx controls the getUpdates loop. Cancelling it stops taking on new
//     work, which is what a shutdown signal should do first.
//   - workCtx controls the updates already being handled. It is cancelled only
//     when the grace period expires, so a restart does not kill a Codex turn in
//     the middle of editing files. Codex writes to the working tree; an
//     interrupted turn can leave a half-applied change, which is worse than
//     waiting a few seconds for it to finish.
//
// Run returns when polling stops. Handlers may still be running; Wait reports
// when they are not.
func (b *Bot) Run(pollCtx, workCtx context.Context) error {
	b.setWorkContext(workCtx)
	if err := b.Prepare(pollCtx); err != nil {
		return err
	}
	retryCtx, cancelRetry := context.WithCancel(pollCtx)
	defer cancelRetry()
	b.wg.Add(1)
	go func() {
		defer b.wg.Done()
		b.retryLoop(retryCtx)
	}()
	return b.Poll(pollCtx)
}

func (b *Bot) retryLoop(ctx context.Context) {
	b.retryPendingReplies(ctx)
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			b.retryPendingReplies(ctx)
		}
	}
}

// retryPendingReplies handles completed turns whose offset has already moved
// past the original update. Waiting beyond the normal post-turn deadline avoids
// racing the original handler's delivery attempt.
func (b *Bot) retryPendingReplies(ctx context.Context) {
	pending, err := b.st.PendingReplies(ctx, b.pendingCursor, 20)
	if err != nil {
		b.log.Error("could not list undelivered replies", "error", err.Error())
		return
	}
	cutoff := b.now().Add(-afterTurnTimeout - 10*time.Second)
	for _, reply := range pending {
		b.pendingCursor = reply.TurnID
		if reply.FinishedAt.After(cutoff) {
			continue
		}
		sendCtx, cancel := context.WithTimeout(ctx, afterTurnTimeout)
		scope := Scope{ChatID: reply.ChatID, ThreadID: reply.ThreadID, UserID: reply.UserID}
		body := reply.Reply
		if reply.Legacy {
			label := fmt.Sprintf("Recovered reply for session %s", reply.SessionID)
			if !reply.TopicKnown {
				label += " (original topic unavailable; sent to this private chat)"
			}
			body = label + ":\n\n" + body
		}
		err := b.deliverReply(sendCtx, scope, body, reply.StartedAt)
		if err == nil {
			err = b.st.MarkTurnDelivered(sendCtx, reply.TurnID)
		}
		cancel()
		if err != nil {
			b.log.Warn("could not retry an undelivered reply", "turn_id", reply.TurnID, "error", err.Error())
		} else {
			b.log.Info("delivered a saved reply", "turn_id", reply.TurnID, "update_id", reply.UpdateID)
		}
	}
}

// Start is Run with one context, for callers that do not need the split.
func (b *Bot) Start(ctx context.Context) error { return b.Run(ctx, ctx) }
