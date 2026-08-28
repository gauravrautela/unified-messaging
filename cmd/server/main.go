// Command server runs the Outlook mail API POC: one process holding the REST
// surface, the OAuth connect flow, the sync engine, and webhook delivery.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/gauravrautela/unified-messaging/internal/accounts"
	"github.com/gauravrautela/unified-messaging/internal/api"
	"github.com/gauravrautela/unified-messaging/internal/auth"
	"github.com/gauravrautela/unified-messaging/internal/chatsync"
	"github.com/gauravrautela/unified-messaging/internal/config"
	"github.com/gauravrautela/unified-messaging/internal/events"
	"github.com/gauravrautela/unified-messaging/internal/logx"
	"github.com/gauravrautela/unified-messaging/internal/model"
	"github.com/gauravrautela/unified-messaging/internal/notify"
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

	// Keys logx.Digest, the one-way handle chat ids and phone numbers are logged
	// through. Unkeyed it is invertible by table lookup over a numbering plan;
	// keyed with the same secret that seals tokens, handles still correlate
	// across restarts and deployments but say nothing to anyone else.
	logx.SetDigestKey(cfg.TokenKey)

	db, err := store.Open(cfg.DBDriver, cfg.DBDSN)
	if err != nil {
		return err
	}
	defer db.Close()
	db.SetMaxOpenConns(cfg.DBMaxOpenConns)
	// The same key that seals OAuth tokens seals a telegram hook's bot token;
	// a hook can't be saved without it.
	db.SetSealKey(cfg.TokenKey)
	db.SetLogger(log.With("component", "store"))
	db.PurgeExpiredOAuthStates()
	db.PurgeDeadDeliveries(time.Now().Add(-cfg.DeliveryRetention))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// oauth_states was previously purged only at boot, so a long-running
	// process accumulated expired rows for its whole life; dead deliveries
	// carry a full message payload each and were never purged at all. An
	// hourly sweep bounds both, stopped by the same signal context that stops
	// everything else. purgeDone is closed once the loop actually exits, so
	// shutdown can wait for it rather than let db.Close() (deferred above)
	// race a purge query still in flight against the same connection.
	purgeDone := make(chan struct{})
	go func() {
		defer close(purgeDone)
		t := time.NewTicker(time.Hour)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				db.PurgeExpiredOAuthStates()
				n, err := db.PurgeDeadDeliveries(time.Now().Add(-cfg.DeliveryRetention))
				if err != nil {
					log.Error("purging dead deliveries", "err", err)
					continue
				}
				log.Info("purge", "dead_deliveries", n)
			}
		}
	}()

	// The dispatcher outlives the signal context on purpose. Cancelling both at
	// the same instant is what made the shutdown "drain" a no-op: the delivery
	// worker returned with events still queued, and every in-flight POST was
	// aborted mid-request. dispCancel runs only after the chat runtime has
	// stopped producing events. Spec §5: HTTP -> Runtime.Wait (<=10s) ->
	// dispatcher drain.
	dispCtx, dispCancel := context.WithCancel(context.Background())
	defer dispCancel()
	senders := notify.NewRegistry(nil)
	dispatcher := events.NewDispatcher(db, senders, log)
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
	// device keys unsealed in the database, so it only joins the registry
	// when an operator has deliberately turned it on. db.Dialect() is
	// whatsmeow's own name for whichever engine db is actually running on
	// ("sqlite3" or "postgres"), never assumed.
	if cfg.WhatsAppEnabled {
		wa, err := whatsapp.New(db.DB(), db.Dialect(), cfg.WhatsAppDeviceName, log)
		if err != nil {
			return err
		}
		registry.Add(wa)
	}
	acctMgr.SetRegistry(registry)

	authSvc := auth.New(db, log, cfg.SessionTTL, cfg.SessionMaxAge)

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
		Handler:           api.NewServer(cfg, db, registry, acctMgr, sync, authSvc, chat, dispatcher, senders, log).Routes(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    64 << 10,
	}

	fields := []any{"addr", cfg.ListenAddr}
	fields = append(fields, dbLogFields(cfg)...)
	fields = append(fields,
		"providers", registry.Names(),
		"push_notifications", cfg.PushEnabled(),
		"public_base_url", cfg.PublicBaseURL,
		"tenant", cfg.Tenant,
		"redirect_uri", cfg.RedirectURI,
		"scopes", cfg.Scopes,
		"client_secret_set", cfg.ClientSecret != "",
		"session_ttl", cfg.SessionTTL,
		"session_max_age", cfg.SessionMaxAge,
		"backfill", opts.BackfillWindow,
		"poll_every", opts.PollInterval,
		"whatsapp", cfg.WhatsAppEnabled,
		"max_chat_accounts", cfg.WhatsAppMaxAccounts,
		"debug", os.Getenv("DEBUG") != "")

	go func() {
		log.Info("listening", fields...)
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
	// ctx (the signal context) is already cancelled by this point, so the
	// purge loop has already seen <-ctx.Done() and is on its way out; this
	// just makes sure it has actually finished before the deferred db.Close()
	// runs, the same way dispatcher.Wait() bounds the dispatcher above.
	<-purgeDone
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

// dbLogFields describes the database at startup without ever logging the DSN:
// a postgres URL carries the password, so only the host and database name are
// reported. On sqlite the file path is not a secret and is logged as before.
func dbLogFields(cfg *config.Config) []any {
	f := []any{"db_driver", cfg.DBDriver}
	if cfg.DBDriver != "postgres" {
		return append(f, "db", cfg.DBPath)
	}
	u, err := url.Parse(cfg.DBDSN)
	if err != nil {
		return append(f, "db_host", "unparseable")
	}
	return append(f, "db_host", u.Hostname(), "db_name", strings.TrimPrefix(u.Path, "/"))
}
