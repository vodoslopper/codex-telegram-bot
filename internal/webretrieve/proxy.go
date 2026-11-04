package webretrieve

import (
	"context"
	"io"
	"net"
	"net/http"
	"sync/atomic"
	"time"
)

// browserProxy bounds network bytes across the page and its subresources and
// validates every upstream connection, including HTTPS tunnels.
func browserProxy(ctx context.Context) (string, *atomic.Int64, func(), error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", nil, nil, err
	}
	remaining := &atomic.Int64{}
	remaining.Store(MaxDownload)
	transport := &http.Transport{DialContext: publicDial}
	server := &http.Server{ReadHeaderTimeout: 5 * time.Second}
	server.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if remaining.Load() <= 0 {
			http.Error(w, "download limit", 413)
			return
		}
		if r.Method == "CONNECT" {
			upstream, err := publicDial(ctx, "tcp", r.Host)
			if err != nil {
				http.Error(w, err.Error(), 403)
				return
			}
			downstream, buf, err := w.(http.Hijacker).Hijack()
			if err != nil {
				upstream.Close()
				return
			}
			defer downstream.Close()
			defer upstream.Close()
			upstream.SetDeadline(time.Now().Add(40 * time.Second))
			downstream.SetDeadline(time.Now().Add(40 * time.Second))
			buf.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n")
			buf.Flush()
			go func() { io.Copy(upstream, buf); upstream.Close() }()
			io.Copy(budgetWriter{downstream, remaining}, upstream)
			return
		}
		r.RequestURI = ""
		r.Header.Del("Proxy-Authorization")
		resp, err := transport.RoundTrip(r.WithContext(ctx))
		if err != nil {
			http.Error(w, err.Error(), 502)
			return
		}
		defer resp.Body.Close()
		for key, values := range resp.Header {
			for _, v := range values {
				w.Header().Add(key, v)
			}
		}
		w.WriteHeader(resp.StatusCode)
		io.Copy(budgetWriter{w, remaining}, resp.Body)
	})
	go server.Serve(listener)
	return "http://" + listener.Addr().String(), remaining, func() { server.Close(); transport.CloseIdleConnections() }, nil
}

type budgetWriter struct {
	io.Writer
	remaining *atomic.Int64
}

func (w budgetWriter) Write(p []byte) (int, error) {
	n := w.remaining.Add(-int64(len(p))) + int64(len(p))
	if n <= 0 {
		return 0, io.ErrShortWrite
	}
	if int64(len(p)) > n {
		written, _ := w.Writer.Write(p[:n])
		return written, io.ErrShortWrite
	}
	return w.Writer.Write(p)
}
