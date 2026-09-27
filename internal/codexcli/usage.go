package codexcli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

var ErrUsageUnavailable = errors.New("codex usage is unavailable")

// RateWindow is a quota window reported by Codex after a turn.
type RateWindow struct {
	UsedPercent   float64 `json:"used_percent"`
	WindowMinutes int64   `json:"window_minutes"`
	ResetsAt      int64   `json:"resets_at"`
}

// RateLimits is the account quota snapshot in a Codex token_count event.
type RateLimits struct {
	LimitID   string      `json:"limit_id"`
	Primary   *RateWindow `json:"primary"`
	Secondary *RateWindow `json:"secondary"`
}

// UsageSnapshot is the latest token_count event in one Codex thread's rollout.
// It is a saved observation, not a live query of the account.
type UsageSnapshot struct {
	ContextAt     time.Time
	ContextUsed   int64
	ContextWindow int64
	LimitsAt      time.Time
	Limits        *RateLimits
}

type usageLine struct {
	Timestamp time.Time `json:"timestamp"`
	Type      string    `json:"type"`
	Payload   struct {
		Type string `json:"type"`
		Info *struct {
			Last *struct {
				TotalTokens int64 `json:"total_tokens"`
			} `json:"last_token_usage"`
			ModelContextWindow int64 `json:"model_context_window"`
		} `json:"info"`
		RateLimits *RateLimits `json:"rate_limits"`
	} `json:"payload"`
}

// SessionUsage reads a saved Codex thread without running a model turn. Codex
// stores rollouts under CODEX_HOME/sessions/YYYY/MM/DD, ending in the UUID it
// reported to the bot at thread.started.
func (r *Runner) SessionUsage(ctx context.Context, threadID string) (UsageSnapshot, error) {
	if !isUUID(threadID) {
		return UsageSnapshot{}, fmt.Errorf("codexcli: invalid thread id")
	}
	pattern := filepath.Join(r.cfg.CodexHome, "sessions", "*", "*", "*", "*"+threadID+".jsonl")
	paths, err := filepath.Glob(pattern)
	if err != nil {
		return UsageSnapshot{}, fmt.Errorf("codexcli: find rollout: %w", err)
	}
	var latest UsageSnapshot
	for _, path := range paths {
		snapshot, err := readUsageFile(ctx, path)
		if err != nil {
			if errors.Is(err, ErrUsageUnavailable) {
				continue
			}
			return UsageSnapshot{}, err
		}
		if snapshot.ContextAt.After(latest.ContextAt) {
			latest.ContextAt = snapshot.ContextAt
			latest.ContextUsed = snapshot.ContextUsed
			latest.ContextWindow = snapshot.ContextWindow
		}
		if snapshot.LimitsAt.After(latest.LimitsAt) {
			latest.LimitsAt = snapshot.LimitsAt
			latest.Limits = snapshot.Limits
		}
	}
	if latest.ContextAt.IsZero() && latest.LimitsAt.IsZero() {
		return UsageSnapshot{}, ErrUsageUnavailable
	}
	return latest, nil
}

func readUsageFile(ctx context.Context, path string) (UsageSnapshot, error) {
	f, err := os.Open(path)
	if err != nil {
		return UsageSnapshot{}, fmt.Errorf("codexcli: open rollout: %w", err)
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)
	var latest UsageSnapshot
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return UsageSnapshot{}, err
		}
		line := scanner.Bytes()
		if !bytes.Contains(line, []byte(`"token_count"`)) {
			continue
		}
		var event usageLine
		if err := json.Unmarshal(line, &event); err != nil || event.Type != "event_msg" || event.Payload.Type != "token_count" {
			continue
		}
		if event.Payload.Info != nil && event.Payload.Info.Last != nil && event.Payload.Info.ModelContextWindow > 0 && event.Timestamp.After(latest.ContextAt) {
			latest.ContextAt = event.Timestamp
			latest.ContextWindow = event.Payload.Info.ModelContextWindow
			latest.ContextUsed = event.Payload.Info.Last.TotalTokens
		}
		if event.Payload.RateLimits != nil && event.Timestamp.After(latest.LimitsAt) {
			latest.LimitsAt = event.Timestamp
			latest.Limits = event.Payload.RateLimits
		}
	}
	if err := scanner.Err(); err != nil {
		return UsageSnapshot{}, fmt.Errorf("codexcli: scan rollout: %w", err)
	}
	if latest.ContextAt.IsZero() && latest.LimitsAt.IsZero() {
		return UsageSnapshot{}, ErrUsageUnavailable
	}
	return latest, nil
}
