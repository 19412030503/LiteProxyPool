package logic

import (
	"bufio"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/proxy"
)

type Conn = net.Conn

func DialDirect(ctx context.Context, network, addr string, timeout time.Duration) (Conn, error) {
	d := &net.Dialer{Timeout: timeout}
	return d.DialContext(ctx, network, addr)
}

func DialViaProxy(ctx context.Context, node ProxyNode, network, addr string, timeout time.Duration) (Conn, error) {
	if node.Type == "" || node.Addr() == "" {
		return nil, errors.New("invalid proxy node")
	}
	switch node.Type {
	case ProxyTypeSOCKS5:
		return dialViaSOCKS5(ctx, node, network, addr, timeout)
	case ProxyTypeHTTP:
		return dialViaHTTPConnect(ctx, node, network, addr, timeout)
	default:
		return nil, fmt.Errorf("unsupported proxy type: %s", node.Type)
	}
}

func dialViaHTTPConnect(ctx context.Context, node ProxyNode, network, addr string, timeout time.Duration) (Conn, error) {
	if network != "tcp" && network != "tcp4" && network != "tcp6" {
		return nil, fmt.Errorf("http proxy only supports tcp, got %q", network)
	}

	target := addr
	if _, _, err := net.SplitHostPort(target); err != nil {
		// CONNECT requires host:port; default to 443 if port missing.
		target = net.JoinHostPort(target, "443")
	}

	// Variant 1: HTTP/1.1 + common headers.
	conn, code, statusLine, err := httpConnectHandshake(ctx, node, target, timeout, httpConnectOptions{
		httpVersion:     "HTTP/1.1",
		hostHeader:      target,
		proxyConnection: true,
		userAgent:       "LiteProxy/1.0",
	})
	if err == nil {
		return conn, nil
	}
	_ = conn.Close()

	// Variant 2 (compat): HTTP/1.0 + minimal headers (some proxies choke on 1.1/Proxy-Connection).
	if code == http.StatusBadRequest || code == http.StatusMethodNotAllowed || code == http.StatusNotImplemented {
		hostOnly := target
		if h, _, perr := net.SplitHostPort(target); perr == nil {
			hostOnly = h
		}
		conn2, code2, statusLine2, err2 := httpConnectHandshake(ctx, node, target, timeout, httpConnectOptions{
			httpVersion:     "HTTP/1.0",
			hostHeader:      hostOnly,
			proxyConnection: false,
			userAgent:       "",
		})
		if err2 == nil {
			return conn2, nil
		}
		_ = conn2.Close()
		return nil, &httpConnectError{proxy: node.Addr(), target: target, code: code2, statusLine: statusLine2}
	}

	return nil, &httpConnectError{proxy: node.Addr(), target: target, code: code, statusLine: statusLine}
}

func dialViaSOCKS5(ctx context.Context, node ProxyNode, network, addr string, timeout time.Duration) (Conn, error) {
	if network != "tcp" && network != "tcp4" && network != "tcp6" {
		return nil, fmt.Errorf("socks5 upstream only supports tcp, got %q", network)
	}
	var auth *proxy.Auth
	if node.User != "" || node.Pass != "" {
		auth = &proxy.Auth{User: node.User, Password: node.Pass}
	}

	// proxy.SOCKS5 may or may not honor DialContext depending on the forward dialer,
	// so keep a hard timeout at the forward layer.
	forward := &net.Dialer{Timeout: timeout}
	d, err := proxy.SOCKS5("tcp", node.Addr(), auth, forward)
	if err != nil {
		return nil, err
	}

	if cd, ok := d.(proxy.ContextDialer); ok {
		return cd.DialContext(ctx, network, addr)
	}
	return d.Dial(network, addr)
}

func basicAuth(user, pass string) string {
	raw := user + ":" + pass
	return base64.StdEncoding.EncodeToString([]byte(raw))
}

func deadlineFromContext(ctx context.Context, fallback time.Duration) time.Time {
	if dl, ok := ctx.Deadline(); ok {
		return dl
	}
	return time.Now().Add(fallback)
}

type httpConnectOptions struct {
	httpVersion     string
	hostHeader      string
	proxyConnection bool
	userAgent       string
}

type httpConnectError struct {
	proxy      string
	target     string
	code       int
	statusLine string
}

func (e *httpConnectError) Error() string {
	if e == nil {
		return "http connect error"
	}
	if e.statusLine == "" {
		return fmt.Sprintf("proxy connect failed: proxy=%s target=%s code=%d", e.proxy, e.target, e.code)
	}
	return fmt.Sprintf("proxy connect failed: proxy=%s target=%s %s", e.proxy, e.target, e.statusLine)
}

func httpConnectHandshake(ctx context.Context, node ProxyNode, target string, timeout time.Duration, opt httpConnectOptions) (Conn, int, string, error) {
	d := &net.Dialer{Timeout: timeout}
	conn, err := d.DialContext(ctx, "tcp", node.Addr())
	if err != nil {
		return nil, 0, "", err
	}

	deadline := deadlineFromContext(ctx, timeout)
	_ = conn.SetDeadline(deadline)

	br := bufio.NewReader(conn)
	bw := bufio.NewWriter(conn)

	if opt.httpVersion == "" {
		opt.httpVersion = "HTTP/1.1"
	}

	fmt.Fprintf(bw, "CONNECT %s %s\r\n", target, opt.httpVersion)
	if opt.hostHeader != "" {
		fmt.Fprintf(bw, "Host: %s\r\n", opt.hostHeader)
	}
	if opt.proxyConnection {
		fmt.Fprintf(bw, "Proxy-Connection: Keep-Alive\r\n")
	}
	if opt.userAgent != "" {
		fmt.Fprintf(bw, "User-Agent: %s\r\n", opt.userAgent)
	}
	if node.User != "" || node.Pass != "" {
		fmt.Fprintf(bw, "Proxy-Authorization: Basic %s\r\n", basicAuth(node.User, node.Pass))
	}
	fmt.Fprintf(bw, "\r\n")
	if err := bw.Flush(); err != nil {
		return conn, 0, "", err
	}

	statusLine, err := br.ReadString('\n')
	if err != nil {
		return conn, 0, "", err
	}
	statusLine = strings.TrimRight(statusLine, "\r\n")
	parts := strings.SplitN(statusLine, " ", 3)
	if len(parts) < 2 {
		return conn, 0, statusLine, fmt.Errorf("bad proxy response: %q", statusLine)
	}
	code, _ := strconv.Atoi(parts[1])
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return conn, code, statusLine, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}
	}
	if code != http.StatusOK {
		return conn, code, statusLine, errors.New(statusLine)
	}

	_ = conn.SetDeadline(time.Time{})
	return conn, code, statusLine, nil
}
