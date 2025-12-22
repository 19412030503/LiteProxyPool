package logic

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

type ValidationConfig struct {
	Enabled        bool   `json:"enabled"`
	HTTPTestURL    string `json:"http_test_url"`
	SOCKS5TestAddr string `json:"socks5_test_addr"`
	MaxHTTP        int    `json:"max_http"`
	MaxSOCKS5      int    `json:"max_socks5"`
	Concurrency    int    `json:"concurrency"`
}

func (c *ValidationConfig) ApplyDefaults() {
	if c.HTTPTestURL == "" {
		c.HTTPTestURL = "http://example.com/"
	}
	if c.SOCKS5TestAddr == "" {
		c.SOCKS5TestAddr = "example.com:443"
	}
	if c.MaxHTTP == 0 {
		c.MaxHTTP = 500
	}
	if c.MaxSOCKS5 == 0 {
		c.MaxSOCKS5 = 200
	}
	if c.Concurrency <= 0 {
		c.Concurrency = 64
	}
	if c.Concurrency > 256 {
		c.Concurrency = 256
	}
}

type ValidationResult struct {
	ValidHTTP        []ProxyNode
	ValidSOCKS5      []ProxyNode
	TestedHTTP       int
	TestedSOCKS5     int
	ValidHTTPCount   int
	ValidSOCKS5Count int
	Errors           error
}

func ValidateAndFilter(ctx context.Context, nodes []ProxyNode, cfg ValidationConfig, timeout time.Duration) (ValidationResult, error) {
	if !cfg.Enabled {
		return ValidationResult{}, errors.New("validation disabled")
	}
	cfg.ApplyDefaults()

	httpNodes := make([]ProxyNode, 0, 1024)
	socksNodes := make([]ProxyNode, 0, 1024)
	for _, n := range nodes {
		switch n.Type {
		case ProxyTypeHTTP:
			httpNodes = append(httpNodes, n)
		case ProxyTypeSOCKS5:
			socksNodes = append(socksNodes, n)
		}
	}

	var res ValidationResult
	var errList []error

	validHTTP, testedHTTP, err := validateHTTP(ctx, httpNodes, cfg, timeout)
	if err != nil {
		errList = append(errList, fmt.Errorf("http validation: %w", err))
	}
	res.ValidHTTP = validHTTP
	res.TestedHTTP = testedHTTP
	res.ValidHTTPCount = len(validHTTP)

	validSOCKS, testedSOCKS, err := validateSOCKS5(ctx, socksNodes, cfg, timeout)
	if err != nil {
		errList = append(errList, fmt.Errorf("socks5 validation: %w", err))
	}
	res.ValidSOCKS5 = validSOCKS
	res.TestedSOCKS5 = testedSOCKS
	res.ValidSOCKS5Count = len(validSOCKS)

	if len(errList) > 0 {
		res.Errors = errors.Join(errList...)
	}

	merged := MergeDedup(res.ValidSOCKS5, res.ValidHTTP)
	if len(merged) == 0 {
		if res.Errors != nil {
			return res, res.Errors
		}
		return res, errors.New("no valid proxies found")
	}
	return res, res.Errors
}

func validateHTTP(ctx context.Context, candidates []ProxyNode, cfg ValidationConfig, timeout time.Duration) ([]ProxyNode, int, error) {
	keep := cfg.MaxHTTP
	if keep < 0 {
		keep = 0
	}
	testLimit := candidateLimit(len(candidates), keep)
	candidates = candidates[:testLimit]
	return runValidation(ctx, candidates, cfg.Concurrency, keep, func(ctx context.Context, n ProxyNode) (ProxyNode, bool) {
		ok, latency, err := CheckHTTPViaProxy(ctx, n, cfg.HTTPTestURL, timeout)
		if err != nil || !ok {
			return ProxyNode{}, false
		}
		n.LatencyMS = latency
		return n, true
	})
}

func validateSOCKS5(ctx context.Context, candidates []ProxyNode, cfg ValidationConfig, timeout time.Duration) ([]ProxyNode, int, error) {
	keep := cfg.MaxSOCKS5
	if keep < 0 {
		keep = 0
	}
	testLimit := candidateLimit(len(candidates), keep)
	candidates = candidates[:testLimit]
	return runValidation(ctx, candidates, cfg.Concurrency, keep, func(ctx context.Context, n ProxyNode) (ProxyNode, bool) {
		start := time.Now()
		cctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()

		conn, err := DialViaProxy(cctx, n, "tcp", cfg.SOCKS5TestAddr, timeout)
		if err != nil {
			return ProxyNode{}, false
		}
		_ = conn.Close()
		n.LatencyMS = time.Since(start).Milliseconds()
		return n, true
	})
}

func candidateLimit(total int, keep int) int {
	if total <= 0 {
		return 0
	}
	if keep <= 0 {
		if total > 2000 {
			return 2000
		}
		return total
	}
	budget := keep * 10
	if budget < 200 {
		budget = 200
	}
	if budget > 5000 {
		budget = 5000
	}
	if budget > total {
		budget = total
	}
	return budget
}

type validateFn func(ctx context.Context, n ProxyNode) (ProxyNode, bool)

func runValidation(ctx context.Context, candidates []ProxyNode, concurrency int, keep int, fn validateFn) ([]ProxyNode, int, error) {
	if len(candidates) == 0 {
		return nil, 0, nil
	}
	if concurrency <= 0 {
		concurrency = 32
	}
	if concurrency > len(candidates) {
		concurrency = len(candidates)
	}

	type result struct {
		node ProxyNode
		ok   bool
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	workCh := make(chan ProxyNode)
	resCh := make(chan result, concurrency)

	var wg sync.WaitGroup
	wg.Add(concurrency)
	for i := 0; i < concurrency; i++ {
		go func() {
			defer wg.Done()
			for n := range workCh {
				cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
				v, ok := fn(cctx, n)
				cancel()
				select {
				case resCh <- result{node: v, ok: ok}:
				case <-ctx.Done():
					return
				}
			}
		}()
	}

	go func() {
		defer close(workCh)
		for _, n := range candidates {
			select {
			case <-ctx.Done():
				return
			case workCh <- n:
			}
		}
	}()

	go func() {
		wg.Wait()
		close(resCh)
	}()

	out := make([]ProxyNode, 0, minInt(len(candidates), maxInt(keep, 1)))
	tested := 0
	for r := range resCh {
		tested++
		if r.ok {
			out = append(out, r.node)
			if keep > 0 && len(out) >= keep {
				cancel()
			}
		}
	}
	return out, tested, nil
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

