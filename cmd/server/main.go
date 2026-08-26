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
	"github.com/gauravrautela/unified-messaging/internal/auth"
	"github.com/gauravrautela/unified-messaging/internal/chatsync"
	"github.com/gauravrautela/unified-messaging/internal/config"
	"github.com/gauravrautela/unified-messaging/internal/events"
	"github.com/gauravrautela/unified-messaging/internal/model"
	"github.com/gauravrautela/unified-messaging/internal/provider"
	"github.com/gauravrautela/unified-messaging/internal/provider/outlook"
	"github.com/gauravrautela/unified-messaging/internal/provider/whatsapp"
	"github.com/gauravrautela/unified-messaging/internal/store"
	"github.com/gauravrautela/unified-messaging/internal/syncer"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: logLevel()}))
	// Anything that logs from a context with no request logger attached falls
	// back to slog.Default(); without this it would fall back to a handler this
	// process never configured, silently at the wrong level and format.
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

	db, err := store.Open(cfg.DBPath)
	if err != nil {
		return err
	}
	defer db.Close()
	db.SetLogger(log.With("component", "store"))
	db.PurgeExpiredOAuthStates()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// The dispatcher outlives the signal context on purpose. Cancelling both at
	// the same instant is what made the shutdown "drain" a no-op: the delivery
	// worker returned with events still queued, and every in-flight POST was
	// aborted mid-request. dispCancel runs only after the chat runtime has
	// stopped producing events. Spec §5: HTTP -> Runtime.Wait (<=10s) ->
	// dispatcher drain.
	dispCtx, dispCancel := context.WithCancel(context.Background())
	defer dispCancel()
	dispatcher := events.NewDispatcher(db, log)
	dispatcher.Start(dispCtx)

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
	// WhatsApp is opt-in: it opens a socket per linked account and stores
	// device keys unsealed in SQLite, so it only joins the registry when an
	// operator has deliberately turned it on.
	if cfg.WhatsAppEnabled {
		wa, err := whatsapp.New(db.DB(), cfg.WhatsAppDeviceName, log)
		if err != nil {
			return err
		}
		registry.Add(wa)
	}
	acctMgr.SetRegistry(registry)

	authSvc := auth.New(db, log, cfg.SessionTTL)

	opts := syncer.Options{
		BackfillWindow: envDuration("BACKFILL_DAYS", 30*24*time.Hour, 24*time.Hour),
		PollInterval:   envDuration("POLL_INTERVAL_SECONDS", 2*time.Minute, time.Second),
	}
	if cfg.PushEnabled() {
		opts.PublicBaseURL = cfg.PublicBaseURL
	}
	sync := syncer.New(db, registry, acctMgr, dispatcher, log, opts)
	sync.Start(ctx)

	chat := chatsync.New(db, registry, acctMgr, dispatcher, log, chatsync.Options{MaxAccounts: cfg.WhatsAppMaxAccounts})
	chat.Start(ctx)

	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           api.NewServer(cfg, db, registry, acctMgr, sync, authSvc, chat, dispatcher, log).Routes(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		log.Info("listening",
			"addr", cfg.ListenAddr,
			"db", cfg.DBPath,
			"providers", registry.Names(),
			"push_notifications", cfg.PushEnabled(),
			"public_base_url", cfg.PublicBaseURL,
			"tenant", cfg.Tenant,
			"redirect_uri", cfg.RedirectURI,
			"scopes", cfg.Scopes,
			"client_secret_set", cfg.ClientSecret != "",
			"session_ttl", cfg.SessionTTL,
			"backfill", opts.BackfillWindow,
			"poll_every", opts.PollInterval,
			"whatsapp", cfg.WhatsAppEnabled,
			"max_chat_accounts", cfg.WhatsAppMaxAccounts,
			"debug", os.Getenv("DEBUG") != "")
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
	// The signal context is already cancelled here, so the actors are unwinding.
	// whatsmeow's Connect ignores the context it is handed, so an actor stuck in
	// a dial cannot be cancelled — bound the wait rather than let one hung
	// socket carry the process to SIGKILL.
	if !waitBounded(chat.Wait, chatWaitTimeout) {
		log.Warn("chat runtime did not stop in time", "timeout", chatWaitTimeout)
	}
	dispCancel()
	dispatcher.Wait()
	if n := dispatcher.Dropped(); n > 0 {
		log.Warn("events dropped during this process's lifetime", "dropped_total", n)
	}
	return nil
}

// chatWaitTimeout is the spec's bound on Runtime.Wait during shutdown.
const chatWaitTimeout = 10 * time.Second

// waitBounded runs wait and reports whether it finished within d. The goroutine
// it leaks on a timeout dies with the process moments later.
func waitBounded(wait func(), d time.Duration) bool {
	done := make(chan struct{})
	go func() { defer close(done); wait() }()
	select {
	case <-done:
		return true
	case <-time.After(d):
		return false
	}
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
