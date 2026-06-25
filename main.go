// Command 3xui-exporter is a Prometheus exporter for the 3X-UI VPN management
// panel and its underlying Xray-core engine.
//
// It exposes two families of metrics:
//
//	xui_*   - panel data (online users, per-client traffic/limits/expiry)
//	xray_*  - engine data (observatory outbound health, Go runtime memstats)
//
// Data is polled at scrape time via a custom prometheus.Collector, so the
// panel is only queried when Prometheus scrapes /metrics.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/yourusername/3xui-exporter/client"
	"github.com/yourusername/3xui-exporter/collector"
	"github.com/yourusername/3xui-exporter/config"
)

// version is the exporter version. It is overridden at build time via
// -ldflags "-X main.version=<tag>" (see Dockerfile and release build).
var version = "dev"

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	cfg, err := config.Load()
	if err != nil {
		log.Error("failed to load configuration", "err", err)
		os.Exit(1)
	}

	xui, err := client.NewXUIClient(cfg.XUIURI, cfg.XUIUsername, cfg.XUIPassword, cfg.Timeout, cfg.InsecureSkipVerify)
	if err != nil {
		log.Error("failed to create 3X-UI client", "err", err)
		os.Exit(1)
	}

	// xray is nil when XRAY_METRICS_URI is empty, disabling that data source.
	xray := client.NewXrayClient(cfg.XrayMetricsURI, cfg.Timeout)
	if xray == nil {
		log.Warn("XRAY_METRICS_URI is empty; Xray engine metrics are disabled")
	}

	// Use a dedicated registry so we control exactly which collectors are
	// exposed (our exporter plus standard Go process/runtime metrics).
	reg := prometheus.NewRegistry()
	reg.MustRegister(
		collector.New(xui, xray, cfg.Timeout, log),
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)

	mux := http.NewServeMux()
	mux.Handle(cfg.MetricsPath, promhttp.HandlerFor(reg, promhttp.HandlerOpts{
		ErrorHandling: promhttp.ContinueOnError,
	}))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`<html>
<head><title>3X-UI Exporter</title></head>
<body>
<h1>3X-UI Exporter</h1>
<p><a href="` + cfg.MetricsPath + `">Metrics</a></p>
</body>
</html>`))
	})

	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	// Run the server and shut it down gracefully on SIGINT/SIGTERM.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		log.Info("starting 3X-UI exporter",
			"version", version,
			"listen_addr", cfg.ListenAddr,
			"metrics_path", cfg.MetricsPath,
			"panel", cfg.XUIURI,
			"xray_enabled", xray != nil,
		)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("HTTP server failed", "err", err)
			stop()
		}
	}()

	<-ctx.Done()
	log.Info("shutdown signal received, stopping server")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Error("graceful shutdown failed", "err", err)
		os.Exit(1)
	}
	log.Info("server stopped cleanly")
}
