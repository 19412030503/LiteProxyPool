package main

import (
	"context"
	"embed"
	"errors"
	"flag"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	socks5 "github.com/armon/go-socks5"
	"github.com/gin-gonic/gin"

	"lite-proxy/logic"
)

//go:embed static/index.html
var staticFS embed.FS

func main() {
	var socksFixedAddr string
	var socksAutoAddr string
	var webAddr string
	var refreshEvery time.Duration
	var rotateEvery time.Duration
	var dialTimeout time.Duration
	var configPath string

	flag.StringVar(&socksFixedAddr, "socks", "127.0.0.1:1080", "local SOCKS5 (fixed) listen address")
	flag.StringVar(&socksAutoAddr, "socks-auto", "127.0.0.1:1081", "local SOCKS5 (auto) listen address (rotates upstream per connection)")
	flag.StringVar(&webAddr, "web", "127.0.0.1:8088", "web UI/API listen address")
	flag.DurationVar(&refreshEvery, "refresh-every", 30*time.Minute, "refresh proxy pool interval (0 disables)")
	flag.DurationVar(&rotateEvery, "rotate-every", 0, "rotate fixed SOCKS5 upstream interval (0 disables)")
	flag.DurationVar(&dialTimeout, "dial-timeout", 15*time.Second, "upstream dial timeout")
	flag.StringVar(&configPath, "config", "", "path to JSON config (overrides flags when set)")
	flag.Parse()

	logger := log.New(os.Stdout, "", log.LstdFlags)
	fixedManager := logic.NewProxyManager()
	autoManager := logic.NewProxyManagerAuto()

	var cfg Config
	if configPath != "" {
		loaded, err := LoadConfig(configPath)
		if err != nil {
			logger.Fatalf("load config: %v", err)
		}
		loaded.ApplyDefaults()
		if err := loaded.Validate(); err != nil {
			logger.Fatalf("invalid config: %v", err)
		}
		cfg = loaded
		socksFixedAddr = cfg.SOCKSListen
		socksAutoAddr = cfg.SOCKSAutoListen
		webAddr = cfg.WebListen
		refreshEvery = cfg.RefreshEvery.Duration()
		rotateEvery = cfg.RotateEvery.Duration()
		dialTimeout = cfg.DialTimeout.Duration()
	} else {
		ds := logic.DefaultSources()
		cfg = Config{
			SOCKSListen:  socksFixedAddr,
			SOCKSAutoListen: socksAutoAddr,
			WebListen:    webAddr,
			RefreshEvery: DurationValue(refreshEvery),
			RotateEvery:  DurationValue(rotateEvery),
			DialTimeout:  DurationValue(dialTimeout),
			Sources:      &ds,
		}
		cfg.ApplyDefaults()
	}

	dialFixed := func(ctx context.Context, network, addr string) (conn logic.Conn, err error) {
		current, ok := fixedManager.Current()
		if !ok {
			return logic.DialDirect(ctx, network, addr, dialTimeout)
		}
		conn, err = logic.DialViaProxy(ctx, current, network, addr, dialTimeout)
		if err != nil {
			fixedManager.ReportFailure(current, 2)
			return nil, err
		}
		fixedManager.ReportSuccess(current)
		return conn, nil
	}

	dialAuto := func(ctx context.Context, network, addr string) (conn logic.Conn, err error) {
		// SOCKS5 auto listener rotates upstream per connection; fail over a few times.
		const attempts = 3
		for i := 0; i < attempts; i++ {
			current, ok := autoManager.Next()
			if !ok {
				return logic.DialDirect(ctx, network, addr, dialTimeout)
			}
			conn, err = logic.DialViaProxy(ctx, current, network, addr, dialTimeout)
			if err == nil {
				autoManager.ReportSuccess(current)
				return conn, nil
			}
			autoManager.ReportFailure(current, 2)
		}
		return nil, err
	}

	indexHTML, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		logger.Fatalf("read embedded static/index.html: %v", err)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	refresh := logic.NewRefresher([]*logic.ProxyManager{fixedManager, autoManager}, *cfg.Sources, cfg.Proxies, cfg.Validation, dialTimeout)

	go func() {
		// Best-effort initial refresh; keep running even if it fails.
		_, _ = refresh.Refresh(ctx)
		if refreshEvery <= 0 {
			return
		}
		ticker := time.NewTicker(refreshEvery)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				_, _ = refresh.Refresh(ctx)
			}
		}
	}()

	if rotateEvery > 0 {
		hcTarget := cfg.Validation.SOCKS5TestAddr
		hcTimeout := dialTimeout
		hcTLSVerify := cfg.Validation.TLSVerifyEnabled()
		if hcTimeout <= 0 || hcTimeout > 10*time.Second {
			hcTimeout = 10 * time.Second
		}

		ensureValidCurrent := func() {
			tries := fixedManager.PoolSize()
			if tries <= 0 {
				return
			}
			for i := 0; i < tries; i++ {
				current, ok := fixedManager.Current()
				if !ok {
					return
				}
				cctx, cancel := context.WithTimeout(ctx, hcTimeout)
				var ok2 bool
				var err error
				if hcTLSVerify {
					ok2, _, err = logic.CheckSOCKS5TLS(cctx, current, hcTarget, hcTimeout)
				} else {
					ok2, _, err = logic.CheckSOCKS5TCP(cctx, current, hcTarget, hcTimeout)
				}
				cancel()
				if err == nil && ok2 {
					fixedManager.ReportSuccess(current)
					return
				}
				fixedManager.ReportFailure(current, 1)
				_, _ = fixedManager.Next()
			}
		}

		go func() {
			ticker := time.NewTicker(rotateEvery)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					_, _ = fixedManager.Next()
					ensureValidCurrent()
				}
			}
		}()
	}

	// Web (Gin)
	gin.SetMode(gin.ReleaseMode)
	router := gin.New()
	router.Use(gin.Recovery())
	router.Use(func(c *gin.Context) {
		start := time.Now()
		c.Next()
		path := c.Request.URL.Path
		if path == "/api/status" || path == "/healthz" {
			return
		}
		logger.Printf("%s %s %s %d %s", c.ClientIP(), c.Request.Method, path, c.Writer.Status(), time.Since(start).Truncate(time.Millisecond))
	})

	router.GET("/", func(c *gin.Context) {
		c.Data(http.StatusOK, "text/html; charset=utf-8", indexHTML)
	})
	router.GET("/healthz", func(c *gin.Context) {
		c.String(http.StatusOK, "ok\n")
	})

	api := router.Group("/api")
	api.GET("/status", func(c *gin.Context) {
		type apiStatus struct {
			WebListen        string       `json:"web_listen"`
			SOCKSFixedListen string       `json:"socks_fixed_listen"`
			SOCKSAutoListen  string       `json:"socks_auto_listen"`
			Fixed            logic.Status `json:"fixed"`
			Auto             logic.Status `json:"auto"`

			// Backward-compatible fields (fixed).
			CurrentSOCKS5      string    `json:"current_socks5,omitempty"`
			CurrentSOCKS5Index int       `json:"current_socks5_index"`
			SOCKS5PoolSize     int       `json:"socks5_pool_size"`
			PoolSize           int       `json:"pool_size"`
			LastRefreshAt      time.Time `json:"last_refresh_at,omitempty"`
			LastRefreshErr     string    `json:"last_refresh_err,omitempty"`
		}

		fixed := fixedManager.Status()
		auto := autoManager.Status()
		c.JSON(http.StatusOK, apiStatus{
			WebListen:        webAddr,
			SOCKSFixedListen: socksFixedAddr,
			SOCKSAutoListen:  socksAutoAddr,
			Fixed:            fixed,
			Auto:             auto,

			CurrentSOCKS5:      fixed.CurrentSOCKS5,
			CurrentSOCKS5Index: fixed.CurrentSOCKS5Index,
			SOCKS5PoolSize:     fixed.SOCKS5PoolSize,
			PoolSize:           fixed.PoolSize,
			LastRefreshAt:      fixed.LastRefreshAt,
			LastRefreshErr:     fixed.LastRefreshErr,
		})
	})
	api.POST("/next", func(c *gin.Context) {
		next, ok := fixedManager.Next()
		if !ok {
			c.JSON(http.StatusConflict, gin.H{"status": "empty_pool"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"status": "ok", "type": logic.ProxyTypeSOCKS5, "new_proxy": next.String()})
	})
	api.POST("/refresh", func(c *gin.Context) {
		rctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
		defer cancel()
		count, err := refresh.Refresh(rctx)
		if err != nil && count > 0 {
			c.JSON(http.StatusOK, gin.H{"count": count, "warning": err.Error()})
			return
		}
		if err != nil {
			c.JSON(http.StatusBadGateway, gin.H{"count": count, "error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"count": count})
	})
	api.POST("/check", func(c *gin.Context) {
		rctx, cancel := context.WithTimeout(c.Request.Context(), 20*time.Second)
		defer cancel()

		mode := c.Query("mode")
		if mode == "" {
			mode = "fixed"
		}

		var (
			current logic.ProxyNode
			ok      bool
		)
		switch mode {
		case "fixed":
			current, ok = fixedManager.Current()
		case "auto":
			current, ok = autoManager.Current()
			if !ok && autoManager.PoolSize() > 0 {
				current, ok = autoManager.Next()
			}
		default:
			c.JSON(http.StatusBadRequest, gin.H{"valid": false, "error": "invalid mode"})
			return
		}
		if !ok {
			c.JSON(http.StatusConflict, gin.H{"valid": false, "error": "empty_pool"})
			return
		}
		target := c.Query("target")
		if target == "" {
			target = "example.com:443"
		}

		tlsParam := c.Query("tls")
		tlsVerify := false
		switch tlsParam {
		case "1", "true", "yes", "on":
			tlsVerify = true
		case "0", "false", "no", "off":
			tlsVerify = false
		default:
			_, _, port, err := logic.ParseTargetAddr(target)
			if err == nil && port == "443" {
				tlsVerify = true
			}
		}

		start := time.Now()
		var (
			ok2 bool
			err error
		)
		if tlsVerify {
			ok2, _, err = logic.CheckSOCKS5TLS(rctx, current, target, dialTimeout)
		} else {
			ok2, _, err = logic.CheckSOCKS5TCP(rctx, current, target, dialTimeout)
		}
		latency := time.Since(start).Milliseconds()
		if err != nil {
			if mode == "fixed" {
				fixedManager.ReportFailure(current, 1)
			} else {
				autoManager.ReportFailure(current, 1)
			}
			c.JSON(http.StatusOK, gin.H{"valid": false, "latency": latency, "type": logic.ProxyTypeSOCKS5, "proxy": current.String(), "target": target, "tls_verify": tlsVerify, "error": err.Error()})
			return
		}
		if !ok2 {
			c.JSON(http.StatusOK, gin.H{"valid": false, "latency": latency, "type": logic.ProxyTypeSOCKS5, "proxy": current.String(), "target": target, "tls_verify": tlsVerify, "error": "check failed"})
			return
		}
		if mode == "fixed" {
			fixedManager.ReportSuccess(current)
		} else {
			autoManager.ReportSuccess(current)
		}
		c.JSON(http.StatusOK, gin.H{"valid": true, "latency": latency, "type": logic.ProxyTypeSOCKS5, "proxy": current.String(), "target": target, "tls_verify": tlsVerify})
	})
	api.GET("/pool", func(c *gin.Context) {
		mode := c.Query("mode")
		if mode == "" {
			mode = "fixed"
		}
		var (
			nodes []logic.ProxyNode
			size  int
		)
		switch mode {
		case "fixed":
			nodes = fixedManager.PoolSnapshot(200)
			size = fixedManager.PoolSize()
		case "auto":
			nodes = autoManager.PoolSnapshot(200)
			size = autoManager.PoolSize()
		default:
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid mode"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"type": logic.ProxyTypeSOCKS5, "items": nodes, "pool_size": size})
	})

	webServer := &http.Server{Addr: webAddr, Handler: router}
	go func() {
		logger.Printf("web listening on http://%s", webAddr)
		if err := webServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Printf("web server error: %v", err)
			cancel()
		}
	}()

	// SOCKS5 (fixed)
	socksSrvFixed, err := socks5.New(&socks5.Config{
		Logger: logger,
		Dial:   dialFixed,
	})
	if err != nil {
		logger.Fatalf("create socks5 server: %v", err)
	}

	socksLnFixed, err := net.Listen("tcp", socksFixedAddr)
	if err != nil {
		logger.Fatalf("listen socks5 (fixed) %s: %v", socksFixedAddr, err)
	}
	go func() {
		<-ctx.Done()
		_ = socksLnFixed.Close()
	}()
	go func() {
		logger.Printf("socks5 (fixed) listening on %s", socksFixedAddr)
		if err := socksSrvFixed.Serve(socksLnFixed); err != nil {
			if !errors.Is(err, net.ErrClosed) {
				logger.Printf("socks5 server error: %v", err)
				cancel()
			}
		}
	}()

	// SOCKS5 (auto, per-connection rotation)
	socksSrvAuto, err := socks5.New(&socks5.Config{
		Logger: logger,
		Dial:   dialAuto,
	})
	if err != nil {
		logger.Fatalf("create socks5 (auto) server: %v", err)
	}

	socksLnAuto, err := net.Listen("tcp", socksAutoAddr)
	if err != nil {
		logger.Fatalf("listen socks5 (auto) %s: %v", socksAutoAddr, err)
	}
	go func() {
		<-ctx.Done()
		_ = socksLnAuto.Close()
	}()
	go func() {
		logger.Printf("socks5 (auto) listening on %s", socksAutoAddr)
		if err := socksSrvAuto.Serve(socksLnAuto); err != nil {
			if !errors.Is(err, net.ErrClosed) {
				logger.Printf("socks5 (auto) server error: %v", err)
				cancel()
			}
		}
	}()

	<-ctx.Done()

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	_ = webServer.Shutdown(shutdownCtx)
}
