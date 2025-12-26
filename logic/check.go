package logic

import (
	"crypto/tls"
	"context"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"
)

func CheckSOCKS5TCP(ctx context.Context, node ProxyNode, targetAddr string, timeout time.Duration) (valid bool, latencyMS int64, err error) {
	if node.Type != ProxyTypeSOCKS5 {
		return false, 0, fmt.Errorf("unsupported proxy type: %s", node.Type)
	}

	target := targetAddr
	if target == "" {
		target = "example.com:443"
	}
	if _, _, splitErr := net.SplitHostPort(target); splitErr != nil {
		target = net.JoinHostPort(target, "443")
	}

	start := time.Now()
	defer func() { latencyMS = time.Since(start).Milliseconds() }()

	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	conn, err := DialViaProxy(cctx, node, "tcp", target, timeout)
	if err != nil {
		return false, latencyMS, err
	}
	_ = conn.Close()
	return true, latencyMS, nil
}

func CheckSOCKS5TLS(ctx context.Context, node ProxyNode, targetAddr string, timeout time.Duration) (valid bool, latencyMS int64, err error) {
	if node.Type != ProxyTypeSOCKS5 {
		return false, 0, fmt.Errorf("unsupported proxy type: %s", node.Type)
	}

	addr, serverName, port, err := ParseTargetAddr(targetAddr)
	if err != nil {
		return false, 0, err
	}
	if port == "" {
		port = "443"
	}

	start := time.Now()
	defer func() { latencyMS = time.Since(start).Milliseconds() }()

	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	conn, err := DialViaProxy(cctx, node, "tcp", addr, timeout)
	if err != nil {
		return false, latencyMS, err
	}
	defer conn.Close()

	cfg := &tls.Config{
		ServerName: serverName,
	}
	tlsConn := tls.Client(conn, cfg)
	_ = tlsConn.SetDeadline(time.Now().Add(timeout))
	if err := tlsConn.HandshakeContext(cctx); err != nil {
		return false, latencyMS, err
	}
	_ = tlsConn.Close()
	return true, latencyMS, nil
}

func ParseTargetAddr(target string) (addr string, serverName string, port string, err error) {
	target = strings.TrimSpace(target)
	if target == "" {
		target = "example.com:443"
	}

	if strings.Contains(target, "://") {
		u, err := url.Parse(target)
		if err != nil {
			return "", "", "", err
		}
		host := strings.TrimSpace(u.Hostname())
		if host == "" {
			return "", "", "", fmt.Errorf("invalid target: missing host")
		}
		p := u.Port()
		if p == "" {
			switch strings.ToLower(u.Scheme) {
			case "http":
				p = "80"
			default:
				p = "443"
			}
		}
		return net.JoinHostPort(host, p), host, p, nil
	}

	host, p, splitErr := net.SplitHostPort(target)
	if splitErr != nil {
		host = target
		p = "443"
	}
	host = strings.TrimSpace(host)
	p = strings.TrimSpace(p)
	if host == "" {
		return "", "", "", fmt.Errorf("invalid target: empty host")
	}
	if p == "" {
		p = "443"
	}
	return net.JoinHostPort(host, p), host, p, nil
}
