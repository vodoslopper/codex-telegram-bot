package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"path/filepath"
	"strconv"
)

// SendDocument uploads a local file as a Telegram document, preserving formats
// such as transparent PNGs without photo conversion.
func (c *Client) SendDocument(ctx context.Context, chatID, threadID int64, name string, data []byte) (*SentMessage, error) {
	if len(data) == 0 || len(data) > 50<<20 {
		return nil, errors.New("telegram: document must be nonempty and no larger than 50 MB")
	}
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	if err := mw.WriteField("chat_id", strconv.FormatInt(chatID, 10)); err != nil {
		return nil, err
	}
	if threadID != 0 {
		if err := mw.WriteField("message_thread_id", strconv.FormatInt(threadID, 10)); err != nil {
			return nil, err
		}
	}
	part, err := mw.CreateFormFile("document", filepath.Base(name))
	if err != nil {
		return nil, err
	}
	if _, err := part.Write(data); err != nil {
		return nil, fmt.Errorf("telegram: read document: %w", err)
	}
	if err := mw.Close(); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint("sendDocument"), &body)
	if err != nil {
		return nil, fmt.Errorf("telegram: build sendDocument request: %w", err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("User-Agent", "codex-telegram-bot/1.0")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, c.transportError("sendDocument", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody))
	if err != nil {
		return nil, fmt.Errorf("telegram: read sendDocument response: %w", err)
	}
	var env apiResponse[json.RawMessage]
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("telegram: sendDocument returned a non-JSON response (HTTP %d)", resp.StatusCode)
	}
	if !env.OK {
		code := env.ErrorCode
		if code == 0 {
			code = resp.StatusCode
		}
		return nil, &APIError{Method: "sendDocument", ErrorCode: code, Description: c.redact(env.Description)}
	}
	var sent SentMessage
	if err := json.Unmarshal(env.Result, &sent); err != nil {
		return nil, fmt.Errorf("telegram: decode sendDocument result: %w", err)
	}
	return &sent, nil
}
