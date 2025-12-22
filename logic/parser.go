package logic

import (
	"net"
	"net/url"
	"strconv"
	"strings"
)

// ParseProxySpec parses:
// - socks5://ip:port
// - http://ip:port (https:// treated as http)
// - user:pass@ip:port
// - ip:port
//
// If the spec has no scheme, defaultType is used when it's "http" or "socks5".
// If defaultType is empty/"auto", a small heuristic is applied.
func ParseProxySpec(spec string, defaultType string) (ProxyNode, bool) {
	spec = strings.TrimSpace(spec)
	if spec == "" || strings.HasPrefix(spec, "#") {
		return ProxyNode{}, false
	}

	// Scheme-aware parse first.
	if strings.Contains(spec, "://") {
		u, err := url.Parse(spec)
		if err != nil {
			return ProxyNode{}, false
		}
		scheme := strings.ToLower(u.Scheme)
		switch scheme {
		case "http", "https":
			scheme = ProxyTypeHTTP
		case "socks5", "socks5h":
			scheme = ProxyTypeSOCKS5
		default:
			return ProxyNode{}, false
		}

		host := u.Hostname()
		port := u.Port()
		if net.ParseIP(host) == nil || !validPort(port) {
			return ProxyNode{}, false
		}

		user := ""
		pass := ""
		if u.User != nil {
			user = u.User.Username()
			pass, _ = u.User.Password()
		}

		id := host + ":" + port
		return ProxyNode{
			ID:        id,
			Type:      scheme,
			IP:        host,
			Port:      port,
			User:      user,
			Pass:      pass,
			LatencyMS: -1,
		}, true
	}

	// No scheme: allow user:pass@host:port and host:port.
	defaultType = strings.ToLower(strings.TrimSpace(defaultType))

	rawHostport := spec
	userinfo := ""
	if at := strings.LastIndex(spec, "@"); at > 0 {
		userinfo = spec[:at]
		rawHostport = spec[at+1:]
	}

	ip, port, ok := splitHostPortLoose(rawHostport)
	if !ok {
		return ProxyNode{}, false
	}

	pt := defaultType
	if pt != ProxyTypeHTTP && pt != ProxyTypeSOCKS5 {
		pt = guessProxyType(port)
	}

	user := ""
	pass := ""
	if userinfo != "" {
		if parts := strings.SplitN(userinfo, ":", 2); len(parts) == 2 {
			user = parts[0]
			pass = parts[1]
		}
	}

	id := ip + ":" + port
	return ProxyNode{
		ID:        id,
		Type:      pt,
		IP:        ip,
		Port:      port,
		User:      user,
		Pass:      pass,
		LatencyMS: -1,
	}, true
}

func guessProxyType(port string) string {
	// Heuristic: common SOCKS ports.
	switch port {
	case "1080", "1081", "1085", "9050", "9150", "4145":
		return ProxyTypeSOCKS5
	default:
		return ProxyTypeHTTP
	}
}

func validPort(s string) bool {
	n, err := strconv.Atoi(s)
	if err != nil {
		return false
	}
	return n >= 1 && n <= 65535
}

