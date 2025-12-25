package logic

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

type ProxySource struct {
	URL  string `json:"url"`
	Type string `json:"type,omitempty"` // socks5 | auto (or empty)
}

func (s ProxySource) Validate() error {
	switch strings.ToLower(strings.TrimSpace(s.Type)) {
	case "", "auto", ProxyTypeSOCKS5:
		return nil
	default:
		return fmt.Errorf("unsupported source type: %q", s.Type)
	}
}

type Sources []ProxySource

func (s *Sources) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) == 0 || bytes.Equal(b, []byte("null")) {
		*s = nil
		return nil
	}
	if len(b) > 0 && b[0] == '[' {
		var asStrings []string
		if err := json.Unmarshal(b, &asStrings); err == nil {
			out := make([]ProxySource, 0, len(asStrings))
			for _, u := range asStrings {
				u = strings.TrimSpace(u)
				if u == "" {
					continue
				}
				out = append(out, ProxySource{URL: u})
			}
			*s = out
			return nil
		}
		var asObjects []ProxySource
		if err := json.Unmarshal(b, &asObjects); err != nil {
			return err
		}
		*s = asObjects
		return nil
	}
	return fmt.Errorf("sources must be an array")
}

func DefaultSources() Sources {
	return Sources{
		{URL: DefaultSOCKS5Source, Type: ProxyTypeSOCKS5},
	}
}
