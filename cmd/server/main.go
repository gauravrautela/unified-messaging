// Command server runs the Outlook mail API POC: one process holding the REST
// surface, the OAuth connect flow, the sync engine, and webhook delivery.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/gauravrautela/unified-messaging/internal/accounts"
	"github.com/gauravrautela/unified-messaging/internal/api"
	"github.com/gauravrautela/unified-messaging/internal/config"
	"github.com/gauravrautela/unified-messaging/internal/events"
	"github.com/gauravrautela/unified-messaging/internal/model"
	"github.com/gauravrautela/unified-messaging/internal/provider"
	"github.com/gauravrautela/unified-messaging/internal/provider/outlook"
	"github.com/gauravrautela/unified-messaging/internal/store"
	"github.com/gauravrautela/unified-messaging/internal/syncer"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: logLevel()}))

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

	db, err := store.Open(cfg.DBPath)
	if err != nil {
		return err
	}
	defer db.Close()
	db.PurgeExpiredOAuthStates()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	dispatcher := events.NewDispatcher(db, log)
	dispatcher.Start(ctx)

	// The account manager and the providers are mutually dependent: providers
	// need a token source, and refreshing a token needs the provider that issued
	// it. Construction order breaks the cycle — the manager is built first and
	// learns about the registry once the providers exist.
	acctMgr := accounts.NewManager(db, cfg.TokenKey, log)
	acctMgr.OnStatusChange = func(accountID, status string) {
		dispatcher.Emit(model.Event{
			Type: model.EventAccountError, AccountID: accountID,
			Account: &model.Account{ID: accountID, Status: status},
		})
	}

	registry := provider.NewRegistry(
		outlook.New(
			outlook.NewAuth(cfg.ClientID, cfg.ClientSecret, cfg.Tenant, cfg.RedirectURI, cfg.Scopes),
			acctMgr,
		),
	)
	acctMgr.SetRegistry(registry)

	opts := syncer.Options{
		BackfillWindow: envDuration("BACKFILL_DAYS", 30*24*time.Hour, 24*time.Hour),
		PollInterval:   envDuration("POLL_INTERVAL_SECONDS", 2*time.Minute, time.Second),
	}
	if cfg.PushEnabled() {
		opts.PublicBaseURL = cfg.PublicBaseURL
	}
	sync := syncer.New(db, registry, acctMgr, dispatcher, log, opts)
	sync.Start(ctx)

	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           api.NewServer(cfg, db, registry, acctMgr, sync, log).Routes(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		log.Info("listening",
			"addr", cfg.ListenAddr,
			"providers", registry.Names(),
			"push_notifications", cfg.PushEnabled(),
			"backfill", opts.BackfillWindow,
			"poll_every", opts.PollInterval)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("http server stopped", "err", err)
			stop()
		}
	}()

	<-ctx.Done()
	log.Info("shutting down")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Warn("http shutdown", "err", err)
	}
	dispatcher.Wait()
	return nil
}

func logLevel() slog.Level {
	if os.Getenv("DEBUG") != "" {
		return slog.LevelDebug
	}
	return slog.LevelInfo
}

// envDuration reads a plain number from the environment and scales it by unit,
// so operators write BACKFILL_DAYS=7 rather than a Go duration string.
func envDuration(key string, def, unit time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return def
	}
	return time.Duration(n) * unit
}
