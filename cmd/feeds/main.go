// Command feeds is the shared in-cluster feed aggregator: it polls RSS, Atom
// and JSON Feed sources on behalf of every app and serves normalized JSON
// Feed timelines. No API keys, no vendor, one polite poller.
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

	"github.com/FACorreiaa/FCFeeds/internal/api"
	"github.com/FACorreiaa/FCFeeds/internal/config"
	"github.com/FACorreiaa/FCFeeds/internal/discover"
	"github.com/FACorreiaa/FCFeeds/internal/fetch"
	"github.com/FACorreiaa/FCFeeds/internal/metrics"
	"github.com/FACorreiaa/FCFeeds/internal/poll"
	"github.com/FACorreiaa/FCFeeds/internal/store"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(log)
	if err := run(log); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		return err
	}
	st, err := store.Open(cfg.DBPath)
	if err != nil {
		return err
	}
	defer st.Close()

	client := fetch.NewClient(fetch.NewGuard(nil), fetch.Options{
		Timeout: cfg.FetchTimeout, MaxBody: cfg.MaxBodyBytes, UserAgent: cfg.UserAgent,
	})
	pc := poll.DefaultConfig()
	pc.MinInterval = cfg.MinPoll
	pc.Workers = cfg.Workers
	pc.EvictAfter = cfg.EvictAfter
	poller := poll.New(st, client, pc)
	poller.Log = log

	reg := metrics.New()
	srv := api.New(st, poller, discover.New(client), reg, api.Options{ColdFetchBudget: cfg.ColdFetchBudget})
	srv.Log = log

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pollDone := make(chan struct{})
	go func() {
		defer close(pollDone)
		poller.Run(ctx)
	}()

	httpSrv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", cfg.Addr, "db", cfg.DBPath, "workers", cfg.Workers, "min_poll", cfg.MinPoll.String())
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		stop()
		<-pollDone
		return err
	case <-ctx.Done():
	}
	log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		log.Warn("http shutdown", "err", err)
	}
	<-pollDone
	return nil
}
