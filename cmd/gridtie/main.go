// Command gridtie is a local-first, OpenAI-compatible LLM router: it answers
// from your own hardware when it can and from the cloud when it can't.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"

	"github.com/tpural/gridtie/internal/config"
	"github.com/tpural/gridtie/internal/provider"
	"github.com/tpural/gridtie/internal/router"
	"github.com/tpural/gridtie/internal/server"
)

var version = "dev"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := run(ctx, os.Args[1:])
	stop()
	if err != nil {
		slog.Error("exiting", "err", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("gridtie", flag.ExitOnError)
	configPath := fs.String("config", "/etc/gridtie/config.yaml", "path to the config file")
	showVersion := fs.Bool("version", false, "print the version and exit")
	_ = fs.Parse(args)
	if *showVersion {
		fmt.Println(version)
		return nil
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	client := &http.Client{Transport: &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		ForceAttemptHTTP2:   true,
		MaxIdleConnsPerHost: 16,
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: 10 * time.Second,
	}}
	providers := map[string]*provider.Provider{}
	for name, pc := range cfg.Providers {
		if providers[name], err = provider.New(name, pc, client, log); err != nil {
			return fmt.Errorf("provider %s: %w", name, err)
		}
	}
	rt, err := router.New(cfg, providers)
	if err != nil {
		return err
	}
	reg := prometheus.NewRegistry()
	reg.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	api := server.New(server.Options{
		Router:       rt,
		Client:       client,
		Cooldown:     cfg.Cooldown.Duration,
		WakeOnModels: cfg.WakeOnModels,
		Metrics:      server.NewMetrics(reg),
		Log:          log,
	})
	log.Info("starting", "version", version, "listen", cfg.Listen, "admin", cfg.AdminListen,
		"providers", len(providers), "models", len(rt.Models()))

	servers := []*http.Server{
		{Addr: cfg.Listen, Handler: api, ReadHeaderTimeout: 10 * time.Second},
		{Addr: cfg.AdminListen, Handler: server.Admin(rt, providers, reg), ReadHeaderTimeout: 10 * time.Second},
	}
	errc := make(chan error, len(servers))
	for _, s := range servers {
		go func() {
			if err := s.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
				errc <- err
			}
		}()
	}
	select {
	case <-ctx.Done():
	case err = <-errc:
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, s := range servers {
		_ = s.Shutdown(shutdownCtx)
	}
	return err
}
