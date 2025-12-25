package logic

import (
	"context"
	"errors"
	"fmt"
	"net"
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
	default:
		return nil, fmt.Errorf("unsupported proxy type: %s", node.Type)
	}
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
