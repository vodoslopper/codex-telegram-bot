package telegram

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// MaxDownloadBytes matches the public Bot API's 20 MB download limit.
const MaxDownloadBytes int64 = 20 * 1024 * 1024

var ErrFileTooLarge = errors.New("telegram: file exceeds the download limit")

// DownloadFile obtains a file path through getFile and downloads at most 20 MB.
// No token-bearing URL is included in an error.
func (c *Client) DownloadFile(ctx context.Context, fileID string) ([]byte, error) {
	if fileID == "" {
		return nil, errors.New("telegram: empty file id")
	}
	var file struct {
		FileSize int64  `json:"file_size"`
		FilePath string `json:"file_path"`
	}
	if err := c.call(ctx, "getFile", struct {
		FileID string `json:"file_id"`
	}{fileID}, &file); err != nil {
		return nil, err
	}
	if file.FileSize > MaxDownloadBytes {
		return nil, ErrFileTooLarge
	}
	if file.FilePath == "" {
		return nil, errors.New("telegram: getFile did not return a file path")
	}
	parts := strings.Split(file.FilePath, "/")
	for i, part := range parts {
		if part == "" || part == "." || part == ".." || strings.Contains(part, "\\") {
			return nil, errors.New("telegram: invalid file path")
		}
		parts[i] = url.PathEscape(part)
	}
	endpoint := c.baseURL + "/file/bot" + c.token + "/" + strings.Join(parts, "/")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("telegram: build file download: %w", err)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, c.transportError("downloadFile", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("telegram: downloadFile returned HTTP %d", resp.StatusCode)
	}
	if resp.ContentLength > MaxDownloadBytes {
		return nil, ErrFileTooLarge
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, MaxDownloadBytes+1))
	if err != nil {
		return nil, fmt.Errorf("telegram: read downloaded file: %w", err)
	}
	if int64(len(data)) > MaxDownloadBytes {
		return nil, ErrFileTooLarge
	}
	return data, nil
}
