package bot

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"codex-telegram-bot/internal/codexcli"
	"codex-telegram-bot/internal/store"
)

// cmdUsage reports the latest context observation for the selected thread and
// current account limits where Codex's local app-server can provide them.
// It never starts a model turn.
func (b *Bot) cmdUsage(ctx context.Context, p *Prepared) error {
	var snapshot codexcli.UsageSnapshot
	var haveSnapshot bool
	sess, err := b.st.SelectedSession(ctx, p.Scope.ChatID, p.Scope.ThreadID, p.Scope.UserID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}
	if err == nil && sess.HasThread() {
		snapshot, err = b.cx.SessionUsage(ctx, sess.CodexThreadID)
		if err == nil {
			haveSnapshot = true
		} else if !errors.Is(err, codexcli.ErrUsageUnavailable) {
			b.log.Warn("could not read Codex session usage", "session_id", sess.ID, "error", err.Error())
		}
	}

	limits, liveErr := b.cx.LiveRateLimits(ctx)
	live := liveErr == nil && limits != nil
	if !live {
		if liveErr != nil {
			b.log.Debug("live Codex rate limits unavailable", "error", liveErr.Error())
		}
		if haveSnapshot {
			limits = snapshot.Limits
		}
	}

	var sb strings.Builder
	if haveSnapshot && snapshot.ContextWindow > 0 && !snapshot.ContextAt.IsZero() {
		left := clampPercent(100 * (1 - float64(snapshot.ContextUsed)/float64(snapshot.ContextWindow)))
		fmt.Fprintf(&sb, "Context window: %.0f%% left (%s used / %s)\n",
			left, shortTokens(snapshot.ContextUsed), shortTokens(snapshot.ContextWindow))
		fmt.Fprintf(&sb, "Context as of: %s\n", snapshot.ContextAt.UTC().Format("2 Jan 15:04 UTC"))
	} else {
		sb.WriteString("Context window: unavailable until this session completes a Codex turn\n")
	}
	if limits == nil {
		sb.WriteString("Rate limits: unavailable")
		return b.send(ctx, p.Scope, sb.String())
	}
	if live {
		sb.WriteString("Rate limits: live\n")
	} else {
		fmt.Fprintf(&sb, "Rate limits: saved at %s\n", snapshot.LimitsAt.UTC().Format("2 Jan 15:04 UTC"))
	}
	if limits.Primary != nil {
		fmt.Fprintf(&sb, "%s\n", formatRateWindow("5h limit", limits.Primary))
	}
	if limits.Secondary != nil {
		fmt.Fprintf(&sb, "%s\n", formatRateWindow("Weekly limit", limits.Secondary))
	}
	return b.send(ctx, p.Scope, strings.TrimRight(sb.String(), "\n"))
}

func formatRateWindow(label string, window *codexcli.RateWindow) string {
	left := clampPercent(100 - window.UsedPercent)
	blocks := int(math.Round(left / 5))
	line := fmt.Sprintf("%s: [%s%s] %.0f%% left", label,
		strings.Repeat("█", blocks), strings.Repeat("░", 20-blocks), left)
	if window.ResetsAt > 0 {
		line += " (resets " + time.Unix(window.ResetsAt, 0).UTC().Format("15:04 on 2 Jan") + " UTC)"
	}
	return line
}

func clampPercent(n float64) float64 {
	return math.Max(0, math.Min(100, n))
}

func shortTokens(n int64) string {
	if n < 1_000 {
		return fmt.Sprintf("%d", n)
	}
	return fmt.Sprintf("%.0fK", float64(n)/1_000)
}
