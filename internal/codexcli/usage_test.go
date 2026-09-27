package codexcli

import (
	"bufio"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const usageThread = "0199a213-81c0-7800-8aa1-000000000001"

func TestSessionUsageReadsLatestTokenCount(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, "sessions", "2026", "09", "27")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "rollout-2026-09-27T12-00-00-"+usageThread+".jsonl")
	data := `{"timestamp":"2026-09-27T12:00:00Z","type":"event_msg","payload":{"type":"token_count","info":{"last_token_usage":{"total_tokens":10000},"model_context_window":258400}}}
{"timestamp":"2026-09-27T12:01:00Z","type":"event_msg","payload":{"type":"token_count","info":{"last_token_usage":{"total_tokens":134000},"model_context_window":258400}}}
{"timestamp":"2026-09-27T12:02:00Z","type":"event_msg","payload":{"type":"agent_message","message":"private text"}}
{"timestamp":"2026-09-27T12:03:00Z","type":"event_msg","payload":{"type":"token_count","rate_limits":{"limit_id":"codex","primary":{"used_percent":5,"window_minutes":300,"resets_at":1790555695},"secondary":{"used_percent":1,"window_minutes":10080,"resets_at":1791142495}}}}
`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	r, err := NewRunner(Config{Bin: "codex", Workspace: t.TempDir(), CodexHome: home}, nil)
	if err != nil {
		t.Fatal(err)
	}
	snap, err := r.SessionUsage(context.Background(), usageThread)
	if err != nil {
		t.Fatal(err)
	}
	if snap.ContextUsed != 134000 || snap.ContextWindow != 258400 || snap.Limits.Primary.UsedPercent != 5 || snap.Limits.Secondary.WindowMinutes != 10080 {
		t.Errorf("snapshot = %+v", snap)
	}
	if !snap.ContextAt.Equal(time.Date(2026, 9, 27, 12, 1, 0, 0, time.UTC)) || !snap.LimitsAt.Equal(time.Date(2026, 9, 27, 12, 3, 0, 0, time.UTC)) {
		t.Errorf("observation times = %s, %s", snap.ContextAt, snap.LimitsAt)
	}
}

func TestSessionUsageMissingOrInvalid(t *testing.T) {
	r, err := NewRunner(Config{Bin: "codex", Workspace: t.TempDir(), CodexHome: t.TempDir()}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.SessionUsage(context.Background(), usageThread); !errors.Is(err, ErrUsageUnavailable) {
		t.Errorf("missing thread: %v", err)
	}
	if _, err := r.SessionUsage(context.Background(), "../other"); err == nil {
		t.Fatal("accepted an invalid thread id")
	}
}

func TestReadRPCResponseIgnoresNotifications(t *testing.T) {
	input := "{\"method\":\"account/rateLimits/updated\",\"params\":{}}\n" +
		"{\"id\":2,\"result\":{\"rateLimits\":{\"primary\":{\"usedPercent\":5}}}}\n"
	s := bufio.NewScanner(strings.NewReader(input))
	response, err := readRPCResponse(s, 2)
	if err != nil || !strings.Contains(string(response.Result), "usedPercent") {
		t.Fatalf("response = %+v, %v", response, err)
	}
}

func TestLiveRateLimitsReadsCodexBucket(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fake-app-server")
	script := `#!/bin/sh -eu
if [ "${BOT_SECRET+x}" = x ]; then exit 2; fi
IFS= read -r first
printf '%s\n' '{"method":"account/updated","params":{}}' '{"id":1,"result":{}}'
IFS= read -r second
IFS= read -r third
printf '%s\n' '{"id":2,"result":{"rateLimits":{"primary":{"usedPercent":30}},"rateLimitsByLimitId":{"codex":{"limitId":"codex","primary":{"usedPercent":5,"windowDurationMins":300,"resetsAt":1790555695},"secondary":{"usedPercent":1,"windowDurationMins":10080,"resetsAt":1791142495}}}}}'
`
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("sh", "-n", path).CombinedOutput(); err != nil {
		t.Fatalf("invalid test script: %s: %v", out, err)
	}
	r, err := NewRunner(Config{
		Bin: path, Workspace: t.TempDir(), CodexHome: t.TempDir(),
		BaseEnv: []string{"PATH=" + os.Getenv("PATH"), "BOT_SECRET=must-not-reach-child"},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	limits, err := r.LiveRateLimits(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if limits.LimitID != "codex" || limits.Primary.UsedPercent != 5 || limits.Secondary.WindowMinutes != 10080 {
		t.Errorf("live limits = %+v", limits)
	}
}
