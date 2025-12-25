package logic

import (
	"sync"
	"time"
)

const (
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
	CurrentSOCKS5Index int    `json:"current_socks5_index"`

	SOCKS5PoolSize int `json:"socks5_pool_size"`
	PoolSize       int `json:"pool_size"`

	LastRefreshAt  time.Time `json:"last_refresh_at,omitempty"`
	LastRefreshErr string    `json:"last_refresh_err,omitempty"`
}

type ProxyManager struct {
	mu sync.RWMutex

	pool         []ProxyNode
	currentIndex int
	failures     map[string]int

	lastRefreshAt  time.Time
	lastRefreshErr string
}

func NewProxyManager() *ProxyManager { return &ProxyManager{} }

func (m *ProxyManager) SetPool(nodes []ProxyNode) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.pool = m.pool[:0]
	for _, n := range nodes {
		if n.Type != ProxyTypeSOCKS5 || n.Addr() == "" {
			continue
		}
		m.pool = append(m.pool, n)
	}

	if m.currentIndex >= len(m.pool) {
		m.currentIndex = 0
	}
	m.failures = make(map[string]int, 128)
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
	return len(m.pool)
}

func (m *ProxyManager) PoolSnapshot(limit int) []ProxyNode {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if limit <= 0 || limit > len(m.pool) {
		limit = len(m.pool)
	}
	out := make([]ProxyNode, 0, limit)
	out = append(out, m.pool[:limit]...)
	return out
}

func (m *ProxyManager) Current() (ProxyNode, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if len(m.pool) == 0 {
		return ProxyNode{}, false
	}
	if m.currentIndex < 0 || m.currentIndex >= len(m.pool) {
		return ProxyNode{}, false
	}
	return m.pool[m.currentIndex], true
}

func (m *ProxyManager) Next() (ProxyNode, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.pool) == 0 {
		return ProxyNode{}, false
	}
	m.currentIndex = (m.currentIndex + 1) % len(m.pool)
	return m.pool[m.currentIndex], true
}

func (m *ProxyManager) ReportSuccess(node ProxyNode) {
	key := node.Addr()
	if key == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failures != nil {
		delete(m.failures, key)
	}
}

func (m *ProxyManager) ReportFailure(node ProxyNode, removeAfter int) bool {
	key := node.Addr()
	if key == "" {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failures == nil {
		m.failures = make(map[string]int, 128)
	}
	m.failures[key]++
	if removeAfter <= 0 || m.failures[key] < removeAfter {
		return false
	}
	delete(m.failures, key)
	return m.removeLocked(key)
}

func (m *ProxyManager) Remove(node ProxyNode) bool {
	key := node.Addr()
	if key == "" {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failures != nil {
		delete(m.failures, key)
	}
	return m.removeLocked(key)
}

func (m *ProxyManager) removeLocked(addr string) bool {
	if len(m.pool) == 0 {
		return false
	}
	removed := false
	dst := m.pool[:0]
	for i, n := range m.pool {
		if n.Addr() == addr {
			removed = true
			if i < m.currentIndex {
				m.currentIndex--
			}
			continue
		}
		dst = append(dst, n)
	}
	if !removed {
		return false
	}
	m.pool = dst
	if m.currentIndex < 0 {
		m.currentIndex = 0
	}
	if m.currentIndex >= len(m.pool) && len(m.pool) > 0 {
		m.currentIndex = 0
	}
	return true
}

func (m *ProxyManager) Status() Status {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var curSOCKS5 ProxyNode
	if len(m.pool) > 0 && m.currentIndex >= 0 && m.currentIndex < len(m.pool) {
		curSOCKS5 = m.pool[m.currentIndex]
	}
	return Status{
		CurrentSOCKS5:      curSOCKS5.Addr(),
		CurrentSOCKS5Index: m.currentIndex,
		SOCKS5PoolSize:     len(m.pool),
		PoolSize:           len(m.pool),
		LastRefreshAt:  m.lastRefreshAt,
		LastRefreshErr: m.lastRefreshErr,
	}
}
