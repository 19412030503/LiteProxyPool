package logic

import (
	"context"
	"errors"
	"sync"
	"time"
)

type Refresher struct {
	managers []*ProxyManager
	mu      sync.Mutex

	sources Sources
	proxies []string
	validation ValidationConfig
	timeout    time.Duration
}

func NewRefresher(managers []*ProxyManager, sources Sources, proxies []string, validation ValidationConfig, timeout time.Duration) *Refresher {
	managers = append([]*ProxyManager(nil), managers...)
	return &Refresher{
		managers: managers,
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
		for _, m := range r.managers {
			if m == nil {
				continue
			}
			m.SetRefreshResult(time.Now(), fetchErr)
		}
		return 0, fetchErr
	}

	nodes := MergeDedup(staticNodes, fetched)
	if len(nodes) == 0 {
		err := errors.New("empty proxy list")
		if fetchErr != nil {
			err = fetchErr
		}
		for _, m := range r.managers {
			if m == nil {
				continue
			}
			m.SetRefreshResult(time.Now(), err)
		}
		return 0, err
	}

	if r.validation.Enabled {
		res, verr := ValidateAndFilter(ctx, nodes, r.validation, r.timeout)
		if verr != nil && len(res.ValidSOCKS5) == 0 {
			// Keep existing pool if new pool is unusable.
			for _, m := range r.managers {
				if m == nil {
					continue
				}
				m.SetRefreshResult(time.Now(), verr)
			}
			return 0, verr
		}
		nodes = MergeDedup(res.ValidSOCKS5)
		if len(nodes) == 0 {
			for _, m := range r.managers {
				if m == nil {
					continue
				}
				m.SetRefreshResult(time.Now(), verr)
			}
			return 0, verr
		}
		// Surface partial validation errors as refresh error (warning), but still update pool.
		if verr != nil {
			for _, m := range r.managers {
				if m == nil {
					continue
				}
				m.SetPool(nodes)
				m.SetRefreshResult(time.Now(), verr)
			}
			return len(nodes), verr
		}
	}

	for _, m := range r.managers {
		if m == nil {
			continue
		}
		m.SetPool(nodes)
		m.SetRefreshResult(time.Now(), fetchErr)
	}
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
