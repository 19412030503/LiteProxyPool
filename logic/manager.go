package logic

import (
	"sync"
	"time"
)

const (
	ProxyTypeHTTP   = "http"
	ProxyTypeSOCKS5 = "socks5"
)

type ProxyNode struct {
	ID      string `json:"id"`
	Type    string `json:"type"`
	IP      string `json:"ip"`
	Port    string `json:"port"`
	User    string `json:"user,omitempty"`
	Pass    string `json:"pass,omitempty"`
	Country string `json:"country,omitempty"`

	LatencyMS int64 `json:"latency"`
}

func (n ProxyNode) Addr() string {
	if n.IP == "" || n.Port == "" {
		return ""
	}
	return n.IP + ":" + n.Port
}

func (n ProxyNode) String() string {
	if n.Type == "" {
		return n.Addr()
	}
	return n.Type + "://" + n.Addr()
}

type Status struct {
	CurrentSOCKS5      string `json:"current_socks5,omitempty"`
	CurrentHTTP        string `json:"current_http,omitempty"`
	CurrentSOCKS5Index int    `json:"current_socks5_index"`
	CurrentHTTPIndex   int    `json:"current_http_index"`

	SOCKS5PoolSize int `json:"socks5_pool_size"`
	HTTPPoolSize   int `json:"http_pool_size"`
	PoolSize       int `json:"pool_size"`

	LastRefreshAt  time.Time `json:"last_refresh_at,omitempty"`
	LastRefreshErr string    `json:"last_refresh_err,omitempty"`
}

type ProxyManager struct {
	mu sync.RWMutex

	poolAll []ProxyNode
	poolHTTP   []ProxyNode
	poolSOCKS5 []ProxyNode

	currentHTTPIndex   int
	currentSOCKS5Index int

	lastRefreshAt  time.Time
	lastRefreshErr string
}

func NewProxyManager() *ProxyManager { return &ProxyManager{} }

func (m *ProxyManager) SetPool(nodes []ProxyNode) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.poolAll = append([]ProxyNode(nil), nodes...)
	m.poolHTTP = m.poolHTTP[:0]
	m.poolSOCKS5 = m.poolSOCKS5[:0]
	for _, n := range m.poolAll {
		switch n.Type {
		case ProxyTypeHTTP:
			m.poolHTTP = append(m.poolHTTP, n)
		case ProxyTypeSOCKS5:
			m.poolSOCKS5 = append(m.poolSOCKS5, n)
		}
	}

	if m.currentHTTPIndex >= len(m.poolHTTP) {
		m.currentHTTPIndex = 0
	}
	if m.currentSOCKS5Index >= len(m.poolSOCKS5) {
		m.currentSOCKS5Index = 0
	}
}

func (m *ProxyManager) SetRefreshResult(at time.Time, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lastRefreshAt = at
	if err != nil {
		m.lastRefreshErr = err.Error()
	} else {
		m.lastRefreshErr = ""
	}
}

func (m *ProxyManager) PoolSize() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.poolAll)
}

func (m *ProxyManager) PoolSnapshot(limit int) []ProxyNode {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if limit <= 0 || limit > len(m.poolAll) {
		limit = len(m.poolAll)
	}
	out := make([]ProxyNode, 0, limit)
	out = append(out, m.poolAll[:limit]...)
	return out
}

func (m *ProxyManager) PoolSizeByType(proxyType string) int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	switch proxyType {
	case ProxyTypeHTTP:
		return len(m.poolHTTP)
	case ProxyTypeSOCKS5:
		return len(m.poolSOCKS5)
	default:
		return 0
	}
}

func (m *ProxyManager) PoolSnapshotByType(proxyType string, limit int) []ProxyNode {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var src []ProxyNode
	switch proxyType {
	case ProxyTypeHTTP:
		src = m.poolHTTP
	case ProxyTypeSOCKS5:
		src = m.poolSOCKS5
	default:
		return nil
	}

	if limit <= 0 || limit > len(src) {
		limit = len(src)
	}
	out := make([]ProxyNode, 0, limit)
	out = append(out, src[:limit]...)
	return out
}

func (m *ProxyManager) CurrentByType(proxyType string) (ProxyNode, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	switch proxyType {
	case ProxyTypeHTTP:
		if len(m.poolHTTP) == 0 {
			return ProxyNode{}, false
		}
		if m.currentHTTPIndex < 0 || m.currentHTTPIndex >= len(m.poolHTTP) {
			return ProxyNode{}, false
		}
		return m.poolHTTP[m.currentHTTPIndex], true
	case ProxyTypeSOCKS5:
		if len(m.poolSOCKS5) == 0 {
			return ProxyNode{}, false
		}
		if m.currentSOCKS5Index < 0 || m.currentSOCKS5Index >= len(m.poolSOCKS5) {
			return ProxyNode{}, false
		}
		return m.poolSOCKS5[m.currentSOCKS5Index], true
	default:
		return ProxyNode{}, false
	}
}

func (m *ProxyManager) NextByType(proxyType string) (ProxyNode, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	switch proxyType {
	case ProxyTypeHTTP:
		if len(m.poolHTTP) == 0 {
			return ProxyNode{}, false
		}
		m.currentHTTPIndex = (m.currentHTTPIndex + 1) % len(m.poolHTTP)
		return m.poolHTTP[m.currentHTTPIndex], true
	case ProxyTypeSOCKS5:
		if len(m.poolSOCKS5) == 0 {
			return ProxyNode{}, false
		}
		m.currentSOCKS5Index = (m.currentSOCKS5Index + 1) % len(m.poolSOCKS5)
		return m.poolSOCKS5[m.currentSOCKS5Index], true
	default:
		return ProxyNode{}, false
	}
}

func (m *ProxyManager) Status() Status {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var curHTTP ProxyNode
	if len(m.poolHTTP) > 0 && m.currentHTTPIndex >= 0 && m.currentHTTPIndex < len(m.poolHTTP) {
		curHTTP = m.poolHTTP[m.currentHTTPIndex]
	}
	var curSOCKS5 ProxyNode
	if len(m.poolSOCKS5) > 0 && m.currentSOCKS5Index >= 0 && m.currentSOCKS5Index < len(m.poolSOCKS5) {
		curSOCKS5 = m.poolSOCKS5[m.currentSOCKS5Index]
	}
	return Status{
		CurrentSOCKS5:      curSOCKS5.Addr(),
		CurrentHTTP:        curHTTP.Addr(),
		CurrentSOCKS5Index: m.currentSOCKS5Index,
		CurrentHTTPIndex:   m.currentHTTPIndex,
		SOCKS5PoolSize:     len(m.poolSOCKS5),
		HTTPPoolSize:       len(m.poolHTTP),
		PoolSize:           len(m.poolAll),
		LastRefreshAt:  m.lastRefreshAt,
		LastRefreshErr: m.lastRefreshErr,
	}
}
