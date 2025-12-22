package logic

import (
	"context"
	"errors"
	"sync"
	"time"
)

type Refresher struct {
	manager *ProxyManager
	mu      sync.Mutex

	sources Sources
	proxies []string
	validation ValidationConfig
	timeout    time.Duration
}

func NewRefresher(manager *ProxyManager, sources Sources, proxies []string, validation ValidationConfig, timeout time.Duration) *Refresher {
	return &Refresher{
		manager: manager,
		sources: sources,
		proxies: append([]string(nil), proxies...),
		validation: validation,
		timeout:    timeout,
	}
}

func (r *Refresher) Refresh(ctx context.Context) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	staticNodes := ParseProxySpecs(r.proxies, "auto")
	fetched, fetchErr := FetchFromSources(ctx, r.sources)
	if fetchErr != nil && len(staticNodes) == 0 {
		r.manager.SetRefreshResult(time.Now(), fetchErr)
		return 0, fetchErr
	}

	nodes := MergeDedup(staticNodes, fetched)
	if len(nodes) == 0 {
		err := errors.New("empty proxy list")
		if fetchErr != nil {
			err = fetchErr
		}
		r.manager.SetRefreshResult(time.Now(), err)
		return 0, err
	}

	if r.validation.Enabled {
		res, verr := ValidateAndFilter(ctx, nodes, r.validation, r.timeout)
		if verr != nil && len(res.ValidHTTP) == 0 && len(res.ValidSOCKS5) == 0 {
			// Keep existing pool if new pool is unusable.
			r.manager.SetRefreshResult(time.Now(), verr)
			return 0, verr
		}
		nodes = MergeDedup(res.ValidSOCKS5, res.ValidHTTP)
		if len(nodes) == 0 {
			r.manager.SetRefreshResult(time.Now(), verr)
			return 0, verr
		}
		// Surface partial validation errors as refresh error (warning), but still update pool.
		if verr != nil {
			r.manager.SetPool(nodes)
			r.manager.SetRefreshResult(time.Now(), verr)
			return len(nodes), verr
		}
	}

	r.manager.SetPool(nodes)
	r.manager.SetRefreshResult(time.Now(), fetchErr)
	return len(nodes), fetchErr
}

func ParseProxySpecs(specs []string, defaultType string) []ProxyNode {
	out := make([]ProxyNode, 0, len(specs))
	for _, s := range specs {
		n, ok := ParseProxySpec(s, defaultType)
		if !ok {
			continue
		}
		out = append(out, n)
	}
	return out
}
