// Package webretrieve performs application-owned, bounded public web retrieval.
package webretrieve

import (
	"context"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"syscall"
	"time"
)

const MaxDownload = 16 << 20
const MaxOutput = 64 << 10

type Request struct {
	URL    string `json:"url"`
	Start  int    `json:"start,omitempty"`
	End    int    `json:"end,omitempty"`
	Tail   int    `json:"tail,omitempty"`
	Search string `json:"search,omitempty"`
	Expect string `json:"expect,omitempty"`
}
type Result struct {
	URL               string `json:"url"`
	Method            string `json:"method"`
	Content           string `json:"content,omitempty"`
	DownloadTruncated bool   `json:"download_truncated"`
	OutputTruncated   bool   `json:"output_truncated"`
	Error             string `json:"error,omitempty"`
}

// publicDial checks the actual connection address, including redirects and DNS.
func publicDial(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("no public addresses resolved")
	}
	for _, ip := range ips {
		if !ip.IP.IsGlobalUnicast() || ip.IP.IsPrivate() || ip.IP.IsLoopback() || ip.IP.IsLinkLocalUnicast() {
			return nil, fmt.Errorf("public websites only")
		}
	}
	for _, ip := range ips {
		c, e := (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, network, net.JoinHostPort(ip.IP.String(), port))
		if e == nil {
			return c, nil
		}
		err = e
	}
	return nil, err
}

var pageTitle = regexp.MustCompile(`(?is)<title[^>]*>(.*?)</title>`)

func challenge(s string) bool {
	s = strings.ToLower(html.UnescapeString(s))
	if len(s) > 64<<10 {
		title := pageTitle.FindStringSubmatch(s)
		if len(title) < 2 {
			return false
		}
		s = title[1]
	}
	for _, marker := range []string{"anubis_challenge", "anubis-challenge", "making sure you're not a bot", "checking your browser", "just a moment...", "cf-chl-", "enable javascript", "ddos-guard", "vercel security checkpoint", "incapsula incident id", "verify you are human", "access denied", "chrome-error://", "err_name_not_resolved", "err_connection_", "err_proxy_", "err_tunnel_", "err_cert_", "403 forbidden", "404 not found"} {
		if strings.Contains(s, marker) {
			return true
		}
	}
	return false
}

var scripts = regexp.MustCompile(`(?is)<(script|style)\b[^>]*>.*?</(script|style)>`)
var breaks = regexp.MustCompile(`(?i)<(?:br\s*/?|/p|/div|/tr|/li|/h[1-6])>`)
var tags = regexp.MustCompile(`<[^>]*>`)

func plain(s string) string {
	return html.UnescapeString(tags.ReplaceAllString(breaks.ReplaceAllString(scripts.ReplaceAllString(s, ""), "\n"), ""))
}

type Retriever struct {
	// Client and Browser permit deterministic transport/browser fixtures in tests.
	Client  *http.Client
	Browser func(context.Context, string) (string, bool, error)
}

func (r Retriever) Fetch(ctx context.Context, q Request) (out Result) {
	out.URL = q.URL
	out.Method = "http"
	fail := func(err error) Result { out.Content = ""; out.Error = err.Error(); return out }
	u, err := url.Parse(q.URL)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Hostname() == "" || u.User != nil {
		return fail(fmt.Errorf("a public HTTP(S) URL without credentials is required"))
	}
	if q.Start < 0 || q.End < 0 || q.Tail < 0 || q.End > 0 && q.End < q.Start || q.Tail > 10000 {
		return fail(fmt.Errorf("invalid line selection"))
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	client := r.Client
	if client == nil {
		transport := &http.Transport{DialContext: publicDial}
		defer transport.CloseIdleConnections()
		client = &http.Client{Transport: transport, Timeout: 30 * time.Second, CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return fmt.Errorf("too many redirects")
			}
			return nil
		}}
	}
	req, err := http.NewRequestWithContext(ctx, "GET", q.URL, nil)
	if err != nil {
		return fail(err)
	}
	req.Header.Set("User-Agent", "codex-telegram-bot web retrieval")
	resp, err := client.Do(req)
	if err != nil {
		return fail(err)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, MaxDownload+1))
	resp.Body.Close()
	if err != nil {
		return fail(err)
	}
	out.DownloadTruncated = len(data) > MaxDownload
	if out.DownloadTruncated {
		data = data[:MaxDownload]
	}
	body := string(data)
	isHTML := strings.Contains(resp.Header.Get("Content-Type"), "html") || strings.Contains(strings.ToLower(body[:min(len(body), 512)]), "<html")
	if isHTML && challenge(body) {
		out.Method = "chromium-headless-shell"
		browser := r.Browser
		if browser == nil {
			browser = Browser
		}
		body, out.DownloadTruncated, err = browser(ctx, q.URL)
		if err != nil {
			return fail(fmt.Errorf("browser could not access requested content: %w", err))
		}
		if challenge(body) {
			return fail(fmt.Errorf("browser still cannot access requested content: challenge or access-denied page remains"))
		}
		isHTML = true
	} else if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fail(fmt.Errorf("HTTP %d: requested content unavailable", resp.StatusCode))
	}
	if isHTML {
		body = plain(body)
	}
	if strings.TrimSpace(body) == "" {
		return fail(fmt.Errorf("retrieval returned no readable content"))
	}
	if q.Expect != "" && !strings.Contains(body, q.Expect) {
		return fail(fmt.Errorf("requested content not confirmed: expected text missing (possibly beyond download limit)"))
	}
	lines := strings.Split(body, "\n")
	start, end := max(q.Start, 1), len(lines)
	if q.End > 0 {
		end = min(end, q.End)
	}
	if q.Tail > 0 {
		if out.DownloadTruncated {
			return fail(fmt.Errorf("cannot return true tail: download limit reached"))
		}
		start = max(1, len(lines)-q.Tail+1)
	}
	var b strings.Builder
	for i := start; i <= end; i++ {
		if q.Search != "" && !strings.Contains(lines[i-1], q.Search) {
			continue
		}
		line := fmt.Sprintf("%d: %s\n", i, lines[i-1])
		if b.Len()+len(line) > MaxOutput {
			out.OutputTruncated = true
			b.WriteString(line[:MaxOutput-b.Len()])
			break
		}
		b.WriteString(line)
	}
	out.Content = b.String()
	if out.Content == "" {
		return fail(fmt.Errorf("no requested lines or text matches in downloaded content"))
	}
	return out
}

// Browser retains a separate ephemeral profile and never disables Chromium's sandbox.
func Browser(ctx context.Context, target string) (string, bool, error) {
	profile, err := os.MkdirTemp("", "bot-web-browser-")
	if err != nil {
		return "", false, err
	}
	defer os.RemoveAll(profile)
	ctx, cancel := context.WithTimeout(ctx, 40*time.Second)
	defer cancel()
	proxy, budget, closeProxy, err := browserProxy(ctx)
	if err != nil {
		return "", false, err
	}
	defer closeProxy()
	// Anubis can deny HeadlessChrome before offering its JavaScript challenge.
	// Use a conventional Chromium UA while retaining the browser's full sandbox.
	cmd := exec.CommandContext(ctx, "chromium-headless-shell", "--proxy-server="+proxy, "--proxy-bypass-list=<-loopback>", "--disable-quic", "--user-agent=Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36", "--headless", "--disable-gpu", "--disable-background-networking", "--no-first-run", "--user-data-dir="+profile, "--virtual-time-budget=20000", "--dump-dom", target)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 2 * time.Second
	stderr := &boundedBuffer{}
	cmd.Stderr = stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", false, err
	}
	if err = cmd.Start(); err != nil {
		return "", false, err
	}
	defer syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	data, readErr := io.ReadAll(io.LimitReader(stdout, MaxDownload+1))
	truncated := len(data) > MaxDownload || budget.Load() <= 0
	if truncated || readErr != nil {
		cancel()
	}
	err = cmd.Wait()
	if readErr != nil {
		return "", false, readErr
	}
	if err != nil {
		return "", truncated, fmt.Errorf("%w: %s", err, strings.TrimSpace(string(stderr.data)))
	}
	return string(data), truncated, nil
}

type boundedBuffer struct{ data []byte }

func (b *boundedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	b.data = append(b.data, p...)
	if len(b.data) > 4096 {
		b.data = append([]byte(nil), b.data[len(b.data)-4096:]...)
	}
	return n, nil
}
