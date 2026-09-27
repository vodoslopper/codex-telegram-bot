package codexcli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"time"
)

const rateLimitReadTimeout = 10 * time.Second

type rpcResponse struct {
	ID     int             `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  json.RawMessage `json:"error"`
}

type liveRateWindow struct {
	UsedPercent        float64 `json:"usedPercent"`
	WindowDurationMins int64   `json:"windowDurationMins"`
	ResetsAt           int64   `json:"resetsAt"`
}

type liveRateLimits struct {
	LimitID   string          `json:"limitId"`
	Primary   *liveRateWindow `json:"primary"`
	Secondary *liveRateWindow `json:"secondary"`
}

func (l liveRateLimits) snapshot() *RateLimits {
	out := &RateLimits{LimitID: l.LimitID}
	if l.Primary != nil {
		out.Primary = &RateWindow{UsedPercent: l.Primary.UsedPercent, WindowMinutes: l.Primary.WindowDurationMins, ResetsAt: l.Primary.ResetsAt}
	}
	if l.Secondary != nil {
		out.Secondary = &RateWindow{UsedPercent: l.Secondary.UsedPercent, WindowMinutes: l.Secondary.WindowDurationMins, ResetsAt: l.Secondary.ResetsAt}
	}
	return out
}

// LiveRateLimits asks Codex's local app-server for current account limits.
// The protocol is experimental, so callers should fall back to a saved
// token_count snapshot when this read fails.
func (r *Runner) LiveRateLimits(ctx context.Context) (*RateLimits, error) {
	queryCtx, cancel := context.WithTimeout(ctx, rateLimitReadTimeout)
	defer cancel()
	cmd := exec.CommandContext(queryCtx, r.cfg.Bin, "app-server", "--stdio")
	cmd.Dir = r.cfg.Workspace
	cmd.Env = r.childEnv()
	cmd.Stderr = io.Discard
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("codexcli: app-server stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("codexcli: app-server stdout: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("codexcli: start app-server: %w", err)
	}
	defer func() {
		_ = stdin.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 32*1024), 2*1024*1024)
	encoder := json.NewEncoder(stdin)
	if err := encoder.Encode(map[string]any{
		"id": 1, "method": "initialize", "params": map[string]any{
			"clientInfo": map[string]string{"name": "codex-telegram-bot", "version": "1"},
		},
	}); err != nil {
		return nil, fmt.Errorf("codexcli: initialize app-server: %w", err)
	}
	if _, err := readRPCResponse(scanner, 1); err != nil {
		return nil, err
	}
	if err := encoder.Encode(map[string]any{"method": "initialized", "params": map[string]any{}}); err != nil {
		return nil, fmt.Errorf("codexcli: acknowledge initialization: %w", err)
	}
	if err := encoder.Encode(map[string]any{
		"id": 2, "method": "account/rateLimits/read",
		"params": map[string]any{"excludeResetCreditDetails": true},
	}); err != nil {
		return nil, fmt.Errorf("codexcli: request rate limits: %w", err)
	}
	response, err := readRPCResponse(scanner, 2)
	if err != nil {
		return nil, err
	}
	var result struct {
		RateLimits          liveRateLimits            `json:"rateLimits"`
		RateLimitsByLimitID map[string]liveRateLimits `json:"rateLimitsByLimitId"`
	}
	if err := json.Unmarshal(response.Result, &result); err != nil {
		return nil, fmt.Errorf("codexcli: decode rate limits: %w", err)
	}
	limits := result.RateLimits
	if codex, ok := result.RateLimitsByLimitID["codex"]; ok {
		limits = codex
	}
	if limits.Primary == nil && limits.Secondary == nil {
		return nil, ErrUsageUnavailable
	}
	return limits.snapshot(), nil
}

func readRPCResponse(scanner *bufio.Scanner, wantID int) (rpcResponse, error) {
	for scanner.Scan() {
		var response rpcResponse
		if err := json.Unmarshal(scanner.Bytes(), &response); err != nil {
			continue // an unrelated diagnostic cannot become usage data
		}
		if response.ID != wantID {
			continue // notifications have no id
		}
		if len(response.Error) != 0 && string(response.Error) != "null" {
			return rpcResponse{}, ErrUsageUnavailable
		}
		if len(response.Result) == 0 || string(response.Result) == "null" {
			return rpcResponse{}, ErrUsageUnavailable
		}
		return response, nil
	}
	if err := scanner.Err(); err != nil {
		return rpcResponse{}, fmt.Errorf("codexcli: read app-server response: %w", err)
	}
	return rpcResponse{}, errors.New("codexcli: app-server closed before responding")
}
