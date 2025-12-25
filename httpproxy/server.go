//go:build httpproxy

package httpproxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"lite-proxy/logic"
)

type Server struct {
	Addr       string
	Logger     *log.Logger
	DialTimeout time.Duration

	Manager *logic.ProxyManager

	lnMu sync.Mutex
	ln   net.Listener

	transportMu sync.Mutex
	transports  map[string]*http.Transport
}

func (s *Server) ListenAndServe(ctx context.Context) error {
	if s.Addr == "" {
		s.Addr = "127.0.0.1:18080"
	}
	if s.Logger == nil {
		s.Logger = log.New(io.Discard, "", 0)
	}
	if s.Manager == nil {
		return errors.New("httpproxy: Manager is nil")
	}
	if s.transports == nil {
		s.transports = make(map[string]*http.Transport, 16)
	}

	ln, err := net.Listen("tcp", s.Addr)
	if err != nil {
		return err
	}
	s.lnMu.Lock()
	s.ln = ln
	s.lnMu.Unlock()

	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()

	srv := &http.Server{
		Handler:      http.HandlerFunc(s.serveHTTP),
		ErrorLog:     s.Logger,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 0,
	}
	err = srv.Serve(ln)
	if errors.Is(err, net.ErrClosed) || errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func (s *Server) Shutdown(ctx context.Context) error {
	s.lnMu.Lock()
	ln := s.ln
	s.lnMu.Unlock()
	if ln == nil {
		return nil
	}
	done := make(chan struct{})
	go func() {
		_ = ln.Close()
		close(done)
	}()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-done:
		return nil
	}
}

func (s *Server) serveHTTP(w http.ResponseWriter, r *http.Request) {
	defer func() {
		if rec := recover(); rec != nil {
			s.Logger.Printf("httpproxy panic: %v\n%s", rec, debug.Stack())
			http.Error(w, "internal server error", http.StatusInternalServerError)
		}
	}()

	if r.Method == http.MethodConnect {
		s.handleConnect(w, r)
		return
	}
	s.handleForwardHTTP(w, r)
}

func (s *Server) handleConnect(w http.ResponseWriter, r *http.Request) {
	target := strings.TrimSpace(r.Host)
	if target == "" {
		http.Error(w, "missing CONNECT target", http.StatusBadRequest)
		return
	}
	if _, _, err := net.SplitHostPort(target); err != nil {
		http.Error(w, "CONNECT target must be host:port", http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), s.effectiveDialTimeout())
	defer cancel()

	var (
		upConn logic.Conn
		err    error
		node   logic.ProxyNode
		ok     bool
	)
	for attempt := 0; attempt < 3; attempt++ {
		node, ok = s.Manager.CurrentByType(logic.ProxyTypeHTTP)
		if !ok {
			http.Error(w, "no http proxy available", http.StatusServiceUnavailable)
			return
		}
		upConn, err = logic.DialViaProxy(ctx, node, "tcp", target, s.effectiveDialTimeout())
		if err == nil {
			break
		}
		_, _ = s.Manager.NextByType(logic.ProxyTypeHTTP)
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}

	hj, ok := w.(http.Hijacker)
	if !ok {
		_ = upConn.Close()
		http.Error(w, "hijacking not supported", http.StatusInternalServerError)
		return
	}
	clientConn, buf, err := hj.Hijack()
	if err != nil {
		_ = upConn.Close()
		http.Error(w, "hijack failed", http.StatusInternalServerError)
		return
	}

	// CONNECT established.
	_, _ = buf.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n")
	_ = buf.Flush()

	pipe(clientConn, upConn)
}

func (s *Server) handleForwardHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL == nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	targetURL, err := normalizeForwardURL(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if targetURL.Scheme != "http" {
		http.Error(w, "only http scheme supported (https requires CONNECT)", http.StatusBadRequest)
		return
	}

	outReq := r.Clone(r.Context())
	outReq.RequestURI = ""
	outReq.URL = targetURL
	outReq.Host = targetURL.Host
	removeHopByHopHeaders(outReq.Header)

	canRetry := r.Method == http.MethodGet || r.Method == http.MethodHead
	var (
		resp *http.Response
		roundTripErr  error
		node logic.ProxyNode
		ok   bool
	)
	for attempt := 0; attempt < 3; attempt++ {
		node, ok = s.Manager.CurrentByType(logic.ProxyTypeHTTP)
		if !ok {
			http.Error(w, "no http proxy available", http.StatusServiceUnavailable)
			return
		}
		tr := s.transportFor(node)
		resp, roundTripErr = tr.RoundTrip(outReq)
		if roundTripErr == nil {
			break
		}
		if !canRetry {
			break
		}
		_, _ = s.Manager.NextByType(logic.ProxyTypeHTTP)
	}
	if roundTripErr != nil {
		http.Error(w, roundTripErr.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	removeHopByHopHeaders(resp.Header)
	copyHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

func (s *Server) effectiveDialTimeout() time.Duration {
	if s.DialTimeout > 0 {
		return s.DialTimeout
	}
	return 15 * time.Second
}

func (s *Server) transportFor(node logic.ProxyNode) *http.Transport {
	key := node.String()
	s.transportMu.Lock()
	defer s.transportMu.Unlock()

	if tr, ok := s.transports[key]; ok {
		return tr
	}

	proxyURL := &url.URL{
		Scheme: "http",
		Host:   node.Addr(),
	}
	if node.User != "" || node.Pass != "" {
		proxyURL.User = url.UserPassword(node.User, node.Pass)
	}

	timeout := s.effectiveDialTimeout()
	tr := &http.Transport{
		Proxy: http.ProxyURL(proxyURL),
		DialContext: (&net.Dialer{
			Timeout:   timeout,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     false,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}

	s.transports[key] = tr
	return tr
}

func normalizeForwardURL(r *http.Request) (*url.URL, error) {
	// Proxy-form: GET http://host/path HTTP/1.1
	if r.URL.IsAbs() && r.URL.Host != "" {
		u := *r.URL
		if u.Scheme == "" {
			u.Scheme = "http"
		}
		return &u, nil
	}

	// Origin-form: GET /path HTTP/1.1 with Host header.
	host := strings.TrimSpace(r.Host)
	if host == "" {
		return nil, fmt.Errorf("missing Host")
	}
	u := &url.URL{
		Scheme: "http",
		Host:   host,
		Path:   r.URL.Path,
	}
	u.RawQuery = r.URL.RawQuery
	u.Fragment = ""
	return u, nil
}

func removeHopByHopHeaders(h http.Header) {
	for _, k := range []string{
		"Connection",
		"Proxy-Connection",
		"Keep-Alive",
		"Proxy-Authenticate",
		"Proxy-Authorization",
		"TE",
		"Trailer",
		"Transfer-Encoding",
		"Upgrade",
	} {
		h.Del(k)
	}
}

func copyHeaders(dst, src http.Header) {
	for k, vv := range src {
		dst.Del(k)
		for _, v := range vv {
			dst.Add(k, v)
		}
	}
}

func pipe(a, b net.Conn) {
	defer a.Close()
	defer b.Close()
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(b, a)
		_ = b.SetDeadline(time.Now())
	}()
	go func() {
		defer wg.Done()
		_, _ = io.Copy(a, b)
		_ = a.SetDeadline(time.Now())
	}()
	wg.Wait()
}
