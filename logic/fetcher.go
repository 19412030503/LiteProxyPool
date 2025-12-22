package logic

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"
)

const (
	DefaultHTTPSource   = "https://raw.githubusercontent.com/TheSpeedX/PROXY-List/master/http.txt"
	DefaultSOCKS5Source = "https://raw.githubusercontent.com/TheSpeedX/PROXY-List/master/socks5.txt"
)

func FetchDefaultSources(ctx context.Context) ([]ProxyNode, error) {
	return FetchFromSources(ctx, DefaultSources())
}

func FetchFromSources(ctx context.Context, sources Sources) ([]ProxyNode, error) {
	if len(sources) == 0 {
		return nil, errors.New("no sources")
	}

	var all []ProxyNode
	var errs []error
	okAny := false
	for _, src := range sources {
		nodes, err := FetchFromURL(ctx, src.URL, src.Type)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", src.URL, err))
			continue
		}
		okAny = true
		all = append(all, nodes...)
	}
	all = MergeDedup(all)
	if len(all) == 0 {
		if okAny {
			return nil, errors.New("empty proxy list")
		}
		if len(errs) > 0 {
			return nil, errors.Join(errs...)
		}
		return nil, errors.New("fetch failed")
	}
	if len(errs) > 0 {
		return all, errors.Join(errs...)
	}
	return all, nil
}

func FetchFromURL(ctx context.Context, url string, defaultType string) ([]ProxyNode, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}

	client := &http.Client{Timeout: 20 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("fetch %s: http %d", url, resp.StatusCode)
	}

	scanner := bufio.NewScanner(resp.Body)
	out := make([]ProxyNode, 0, 1024)
	seen := make(map[string]struct{}, 2048) // within this single source

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		node, ok := ParseProxySpec(line, defaultType)
		if !ok {
			continue
		}
		key := node.Type + "|" + node.ID
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}

		out = append(out, node)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func MergeDedup(lists ...[]ProxyNode) []ProxyNode {
	out := make([]ProxyNode, 0, 1024)
	seen := make(map[string]struct{}, 4096)
	for _, list := range lists {
		for _, n := range list {
			if n.IP == "" || n.Port == "" || n.Type == "" {
				continue
			}
			key := n.Type + "|" + n.IP + ":" + n.Port
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			n.ID = n.IP + ":" + n.Port
			if n.LatencyMS == 0 {
				n.LatencyMS = -1
			}
			out = append(out, n)
		}
	}
	return out
}

func splitHostPortLoose(s string) (host, port string, ok bool) {
	if strings.Count(s, ":") == 1 {
		parts := strings.SplitN(s, ":", 2)
		host = strings.TrimSpace(parts[0])
		port = strings.TrimSpace(parts[1])
		if host == "" || port == "" {
			return "", "", false
		}
		if net.ParseIP(host) == nil {
			return "", "", false
		}
		if !validPort(port) {
			return "", "", false
		}
		return host, port, true
	}

	// Fallback for ipv6 like [::1]:1080
	h, p, err := net.SplitHostPort(s)
	if err != nil {
		return "", "", false
	}
	h = strings.Trim(h, "[]")
	if net.ParseIP(h) == nil {
		return "", "", false
	}
	if !validPort(p) {
		return "", "", false
	}
	return h, p, true
}
