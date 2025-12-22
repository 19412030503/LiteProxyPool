package logic

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"
)

// CheckHTTPViaProxy validates a proxy node the way a HTTP client would use it.
// - For HTTP proxies, this uses net/http's Proxy transport (HTTP via absolute-form, HTTPS via CONNECT).
// - For SOCKS5 proxies, this uses a custom DialContext (HTTP/HTTPS both supported).
func CheckHTTPViaProxy(ctx context.Context, node ProxyNode, targetURL string, timeout time.Duration) (valid bool, latencyMS int64, err error) {
	start := time.Now()
	defer func() { latencyMS = time.Since(start).Milliseconds() }()

	u, err := url.Parse(targetURL)
	if err != nil {
		return false, 0, err
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return false, 0, fmt.Errorf("unsupported url scheme: %s", u.Scheme)
	}

	client := &http.Client{
		Timeout: timeout,
	}

	switch node.Type {
	case ProxyTypeHTTP:
		pu := &url.URL{Scheme: "http", Host: node.Addr()}
		if node.User != "" || node.Pass != "" {
			pu.User = url.UserPassword(node.User, node.Pass)
		}
		client.Transport = &http.Transport{
			Proxy: http.ProxyURL(pu),
			DialContext: (&net.Dialer{
				Timeout:   timeout,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			ForceAttemptHTTP2:     false,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
			IdleConnTimeout:       30 * time.Second,
		}
	case ProxyTypeSOCKS5:
		client.Transport = &http.Transport{
			Proxy: nil,
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				return DialViaProxy(ctx, node, network, addr, timeout)
			},
			ForceAttemptHTTP2:     false,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
			IdleConnTimeout:       30 * time.Second,
		}
	default:
		return false, 0, fmt.Errorf("unsupported proxy type: %s", node.Type)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return false, 0, err
	}
	req.Header.Set("User-Agent", "LiteProxy/1.0")

	resp, err := client.Do(req)
	if err != nil {
		return false, 0, err
	}
	defer resp.Body.Close()
	_, _ = io.CopyN(io.Discard, resp.Body, 2048)

	// Accept any non-5xx as "reachable" (proxies often return 3xx/4xx depending on target).
	if resp.StatusCode >= 500 {
		return false, latencyMS, fmt.Errorf("http status %d", resp.StatusCode)
	}
	return true, latencyMS, nil
}

