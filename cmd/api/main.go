// Command api runs the whole pipeline: the ingestion gateway, the worker pool,
// and the dashboard API. One binary, one Postgres (docs/04-ARCHITECTURE.md §1).
// ponytail: gateway and workers share a process. Split them into separate
// deployments when they need to scale independently — the queue boundary between
// them is already in place.
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

	"github.com/beda/enquiry-pipeline/internal/api"
	"github.com/beda/enquiry-pipeline/internal/config"
	"github.com/beda/enquiry-pipeline/internal/llm"
	"github.com/beda/enquiry-pipeline/internal/store"
	"github.com/beda/enquiry-pipeline/internal/worker"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(log)

	if err := run(log); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	st, err := store.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer st.Close()

	if err := st.Migrate(ctx); err != nil {
		return err
	}
	if err := st.Seed(ctx, cfg.RulesSeedFile); err != nil {
		return err
	}

	llmClient, err := llm.New(cfg, log)
	if err != nil {
		return err
	}

	pool := worker.NewPool(st, llmClient, cfg, log)
	go pool.Run(ctx)

	srv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           api.NewServer(st, cfg, llmClient, log, pool.Wake).Routes(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", cfg.HTTPAddr, "workers", cfg.WorkerCount,
			"tier1", cfg.Tier1.String(), "tier2", cfg.Tier2.String())
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		log.Info("shutting down")
	}

	// Give in-flight work a moment to finish; anything still running is released
	// back to the queue by the worker's shutdown path.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}
