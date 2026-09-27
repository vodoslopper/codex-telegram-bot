package bot

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"codex-telegram-bot/internal/codexcli"
)

type usageCodex struct {
	Codex
	snapshot codexcli.UsageSnapshot
	live     *codexcli.RateLimits
	liveErr  error
}

func (u usageCodex) SessionUsage(context.Context, string) (codexcli.UsageSnapshot, error) {
	return u.snapshot, nil
}

func (u usageCodex) LiveRateLimits(context.Context) (*codexcli.RateLimits, error) {
	return u.live, u.liveErr
}

func TestUsageCommandShowsContextAndLiveLimitsWithoutRunningTurn(t *testing.T) {
	h := newHarness(t, harnessOpts{spec: successSpec("ready", "ready")})
	h.text(msg(1, aliceChat, aliceID, "start"))
	h.b.cx = usageCodex{Codex: h.b.cx,
		snapshot: codexcli.UsageSnapshot{ContextAt: time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC), ContextUsed: 134000, ContextWindow: 258400},
		live: &codexcli.RateLimits{
			Primary:   &codexcli.RateWindow{UsedPercent: 5, WindowMinutes: 300, ResetsAt: 1790555695},
			Secondary: &codexcli.RateWindow{UsedPercent: 1, WindowMinutes: 10080, ResetsAt: 1791142495},
		},
	}
	got := h.text(msg(2, aliceChat, aliceID, "/usage"))
	for _, part := range []string{"Context window:", "134K used / 258K", "Rate limits: live", "5h limit:", "95% left", "Weekly limit:", "99% left"} {
		if !strings.Contains(got, part) {
			t.Errorf("usage reply lacks %q: %q", part, got)
		}
	}
	if n := len(h.fake.Invocations(t)); n != 1 {
		t.Errorf("/usage ran %d Codex turns, want only the initial turn", n)
	}
}

func TestUsageCommandFallsBackToSavedLimits(t *testing.T) {
	h := newHarness(t, harnessOpts{spec: successSpec("ready", "ready")})
	h.text(msg(1, aliceChat, aliceID, "start"))
	h.b.cx = usageCodex{Codex: h.b.cx,
		snapshot: codexcli.UsageSnapshot{
			LimitsAt: time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC),
			Limits:   &codexcli.RateLimits{Primary: &codexcli.RateWindow{UsedPercent: 8, WindowMinutes: 300}},
		},
		liveErr: errors.New("app-server unavailable"),
	}
	got := h.text(msg(2, aliceChat, aliceID, "/usage"))
	if !strings.Contains(got, "Rate limits: saved at 27 Sep 12:00 UTC") || !strings.Contains(got, "92% left") {
		t.Errorf("fallback reply = %q", got)
	}
}
