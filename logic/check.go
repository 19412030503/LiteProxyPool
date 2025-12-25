package logic

import (
	"context"
	"fmt"
	"net"
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
