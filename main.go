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
	var socksAddr string
	var webAddr string
	var refreshEvery time.Duration
	var rotateEvery time.Duration
	var dialTimeout time.Duration
	var configPath string

	flag.StringVar(&socksAddr, "socks", "127.0.0.1:1080", "local SOCKS5 listen address")
	flag.StringVar(&webAddr, "web", "127.0.0.1:8088", "web UI/API listen address")
	flag.DurationVar(&refreshEvery, "refresh-every", 30*time.Minute, "refresh proxy pool interval (0 disables)")
	flag.DurationVar(&rotateEvery, "rotate-every", 30*time.Second, "rotate upstream SOCKS5 proxy interval (0 disables)")
	flag.DurationVar(&dialTimeout, "dial-timeout", 15*time.Second, "upstream dial timeout")
	flag.StringVar(&configPath, "config", "", "path to JSON config (overrides flags when set)")
	flag.Parse()

	logger := log.New(os.Stdout, "", log.LstdFlags)
	manager := logic.NewProxyManager()

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
		socksAddr = cfg.SOCKSListen
		webAddr = cfg.WebListen
		refreshEvery = cfg.RefreshEvery.Duration()
		rotateEvery = cfg.RotateEvery.Duration()
		dialTimeout = cfg.DialTimeout.Duration()
	} else {
		ds := logic.DefaultSources()
		cfg = Config{
			SOCKSListen:  socksAddr,
			WebListen:    webAddr,
			RefreshEvery: DurationValue(refreshEvery),
			RotateEvery:  DurationValue(rotateEvery),
			DialTimeout:  DurationValue(dialTimeout),
			Sources:      &ds,
		}
		cfg.ApplyDefaults()
	}

	dial := func(ctx context.Context, network, addr string) (conn logic.Conn, err error) {
		// SOCKS5 listener only uses SOCKS5 upstream pool; fail over a few times.
		const attempts = 3
		for i := 0; i < attempts; i++ {
			current, ok := manager.Current()
			if !ok {
				return logic.DialDirect(ctx, network, addr, dialTimeout)
			}
			conn, err = logic.DialViaProxy(ctx, current, network, addr, dialTimeout)
			if err == nil {
				manager.ReportSuccess(current)
				return conn, nil
			}
			manager.ReportFailure(current, 2)
			_, _ = manager.Next()
		}
		return nil, err
	}

	indexHTML, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		logger.Fatalf("read embedded static/index.html: %v", err)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	refresh := logic.NewRefresher(manager, *cfg.Sources, cfg.Proxies, cfg.Validation, dialTimeout)

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
		if hcTimeout <= 0 || hcTimeout > 10*time.Second {
			hcTimeout = 10 * time.Second
		}

		ensureValidCurrent := func() {
			tries := manager.PoolSize()
			if tries <= 0 {
				return
			}
			for i := 0; i < tries; i++ {
				current, ok := manager.Current()
				if !ok {
					return
				}
				cctx, cancel := context.WithTimeout(ctx, hcTimeout)
				ok2, _, err := logic.CheckSOCKS5TCP(cctx, current, hcTarget, hcTimeout)
				cancel()
				if err == nil && ok2 {
					manager.ReportSuccess(current)
					return
				}
				manager.ReportFailure(current, 1)
				_, _ = manager.Next()
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
					_, _ = manager.Next()
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
		c.JSON(http.StatusOK, manager.Status())
	})
	api.POST("/next", func(c *gin.Context) {
		next, ok := manager.Next()
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
		current, ok := manager.Current()
		if !ok {
			c.JSON(http.StatusConflict, gin.H{"valid": false, "error": "empty_pool"})
			return
		}
		target := c.Query("target")
		if target == "" {
			target = "example.com:443"
		}
		start := time.Now()
		conn, err := logic.DialViaProxy(rctx, current, "tcp", target, dialTimeout)
		latency := time.Since(start).Milliseconds()
		if err != nil {
			manager.ReportFailure(current, 1)
			c.JSON(http.StatusOK, gin.H{"valid": false, "latency": latency, "type": logic.ProxyTypeSOCKS5, "proxy": current.String(), "target": target, "error": err.Error()})
			return
		}
		_ = conn.Close()
		manager.ReportSuccess(current)
		c.JSON(http.StatusOK, gin.H{"valid": true, "latency": latency, "type": logic.ProxyTypeSOCKS5, "proxy": current.String(), "target": target})
	})
	api.GET("/pool", func(c *gin.Context) {
		nodes := manager.PoolSnapshot(200)
		c.JSON(http.StatusOK, gin.H{"type": logic.ProxyTypeSOCKS5, "items": nodes, "pool_size": manager.PoolSize()})
	})

	webServer := &http.Server{Addr: webAddr, Handler: router}
	go func() {
		logger.Printf("web listening on http://%s", webAddr)
		if err := webServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Printf("web server error: %v", err)
			cancel()
		}
	}()

	// SOCKS5 (armon/go-socks5)
	socksSrv, err := socks5.New(&socks5.Config{
		Logger: logger,
		Dial:   dial,
	})
	if err != nil {
		logger.Fatalf("create socks5 server: %v", err)
	}

	socksLn, err := net.Listen("tcp", socksAddr)
	if err != nil {
		logger.Fatalf("listen socks5 %s: %v", socksAddr, err)
	}
	go func() {
		<-ctx.Done()
		_ = socksLn.Close()
	}()
	go func() {
		logger.Printf("socks5 listening on %s", socksAddr)
		if err := socksSrv.Serve(socksLn); err != nil {
			if !errors.Is(err, net.ErrClosed) {
				logger.Printf("socks5 server error: %v", err)
				cancel()
			}
		}
	}()

	<-ctx.Done()

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	_ = webServer.Shutdown(shutdownCtx)
}
