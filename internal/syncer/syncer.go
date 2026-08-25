// Package syncer keeps the local mirror in step with a connected account.
//
// Two mechanisms cooperate. Push notifications give near-real-time wakeups but
// carry no delivery guarantee; a periodic incremental poll is the safety net
// that makes eventual consistency actually eventual. Both funnel into the same
// idempotent walk, so a duplicate wakeup costs one cheap round-trip.
//
// Nothing here knows what a provider is beyond the contracts in
// internal/provider.
package syncer

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/gauravrautela/unified-messaging/internal/accounts"
	"github.com/gauravrautela/unified-messaging/internal/events"
	"github.com/gauravrautela/unified-messaging/internal/logx"
	"github.com/gauravrautela/unified-messaging/internal/model"
	"github.com/gauravrautela/unified-messaging/internal/provider"
	"github.com/gauravrautela/unified-messaging/internal/store"
)

// scopeListCursorKey is a reserved scope_id holding the cursor for scope
// discovery itself, which has no scope of its own.
const scopeListCursorKey = "__scopes__"

type Options struct {
	// BackfillWindow bounds the first sync. Zero means everything available.
	BackfillWindow time.Duration
	// PollInterval is the incremental safety net.
	PollInterval time.Duration
	// PublicBaseURL is the externally reachable https origin providers deliver
	// push notifications to. Empty disables push entirely and leaves the poll
	// carrying the whole load.
	PublicBaseURL string
}

// Push endpoints are namespaced per provider so each one's validation quirks
// and payload format stay addressable.
func (o Options) notificationURL(providerName string) string {
	if o.PublicBaseURL == "" {
		return ""
	}
	return o.PublicBaseURL + "/notifications/" + providerName
}

func (o Options) lifecycleURL(providerName string) string {
	if o.PublicBaseURL == "" {
		return ""
	}
	return o.PublicBaseURL + "/notifications/" + providerName + "/lifecycle"
}

type Syncer struct {
	store    *store.Store
	registry *provider.Registry
	accts    *accounts.Manager
	events   *events.Dispatcher
	log      *slog.Logger
	opts     Options

	wake chan string

	mu       sync.Mutex
	inflight map[string]bool
	pending  map[string]bool
}

func New(s *store.Store, reg *provider.Registry, a *accounts.Manager,
	e *events.Dispatcher, log *slog.Logger, opts Options) *Syncer {
	if opts.PollInterval <= 0 {
		opts.PollInterval = 2 * time.Minute
	}
	return &Syncer{
		store: s, registry: reg, accts: a, events: e, log: log, opts: opts,
		wake:     make(chan string, 256),
		inflight: map[string]bool{},
		pending:  map[string]bool{},
	}
}

func (s *Syncer) Start(ctx context.Context) {
	go s.worker(ctx)
	go s.pollLoop(ctx)
	if s.opts.PublicBaseURL != "" {
		go s.subscriptionLoop(ctx)
	} else {
		s.log.Warn("no public https origin configured; " +
			"push notifications disabled, relying on incremental polling")
	}
}

// Wake requests a sync. Requests for an account already syncing collapse into a
// single follow-up run rather than queueing up.
func (s *Syncer) Wake(accountID string) {
	s.mu.Lock()
	if s.inflight[accountID] {
		s.pending[accountID] = true
		s.mu.Unlock()
		return
	}
	s.mu.Unlock()

	select {
	case s.wake <- accountID:
	default:
		s.log.Warn("sync queue full, dropping wakeup", "account_id", accountID)
	}
}

func (s *Syncer) worker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case id := <-s.wake:
			s.runOnce(ctx, id)
		}
	}
}

func (s *Syncer) runOnce(ctx context.Context, accountID string) {
	s.mu.Lock()
	if s.inflight[accountID] {
		s.pending[accountID] = true
		s.mu.Unlock()
		return
	}
	s.inflight[accountID] = true
	s.mu.Unlock()

	if err := s.SyncAccount(ctx, accountID); err != nil {
		s.log.Error("sync failed", "account_id", accountID, "err", err)
	}

	s.mu.Lock()
	delete(s.inflight, accountID)
	again := s.pending[accountID]
	delete(s.pending, accountID)
	s.mu.Unlock()

	if again && ctx.Err() == nil {
		s.Wake(accountID)
	}
}

func (s *Syncer) pollLoop(ctx context.Context) {
	t := time.NewTicker(s.opts.PollInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			accts, err := s.store.ListAllAccounts()
			if err != nil {
				s.log.Error("listing accounts for poll", "err", err)
				continue
			}
			n := 0
			for _, a := range accts {
				if a.Status == model.AccountOK {
					n++
					s.Wake(a.ID)
				}
			}
			s.log.Debug("poll tick", "component", "syncer", "accounts", len(accts), "ok", n)
		}
	}
}

// mailboxFor resolves the provider that owns an account.
func (s *Syncer) mailboxFor(acct model.Account) (provider.Mailbox, error) {
	p, err := s.registry.Get(acct.Provider)
	if err != nil {
		return nil, err
	}
	return p.Mailbox(), nil
}

// SyncAccount brings one account up to date: discover scopes, then walk each.
func (s *Syncer) SyncAccount(ctx context.Context, accountID string) error {
	// One id ties every line of a run together, the way request_id does for a
	// request, so a run can be read end to end out of a busy log.
	runID := "run_" + strings.TrimPrefix(logx.NewRequestID(), "req_")
	log := s.log.With("component", "syncer", "run_id", runID, "account_id", accountID)
	ctx = logx.With(ctx, log)
	start := time.Now()
	log.Info("sync run started")
	defer func() { log.Info("sync run finished", "dur", time.Since(start).Round(time.Millisecond)) }()

	acct, err := s.store.GetAnyAccount(accountID)
	if err != nil {
		return err
	}
	if acct.Status != model.AccountOK {
		log.Debug("skipping sync for non-OK account", "status", acct.Status)
		return nil
	}
	log = log.With("developer_id", acct.DeveloperID, "provider", acct.Provider)
	ctx = logx.With(ctx, log)
	mailbox, err := s.mailboxFor(acct)
	if err != nil {
		return err
	}
	firstRun := acct.LastSyncedAt == nil
	log.Debug("run decision", "first_run", firstRun,
		"last_synced_at", acct.LastSyncedAt, "backfill_window", s.opts.BackfillWindow)

	scopes, err := s.syncScopes(ctx, mailbox, accountID)
	if err != nil {
		if errors.Is(err, provider.ErrReauthRequired) {
			log.Debug("run aborted", "reason", "reauth required")
			return nil // status already flipped; nothing more to do
		}
		return err
	}
	log.Debug("scopes discovered", "scopes", len(scopes))

	for _, scope := range scopes {
		if err := s.syncScope(ctx, mailbox, accountID, scope, firstRun); err != nil {
			if errors.Is(err, provider.ErrReauthRequired) {
				log.Debug("run aborted", "reason", "reauth required", "scope", scope.Name)
				return nil
			}
			// One bad scope should not stop the rest of the account.
			log.Error("scope sync failed", "scope", scope.Name, "err", err)
		}
	}
	return s.store.MarkSynced(accountID)
}

// syncScopes refreshes the folder table from the provider's scope listing and
// returns the scopes to walk.
func (s *Syncer) syncScopes(ctx context.Context, mailbox provider.Mailbox, accountID string) ([]provider.Scope, error) {
	cursor, err := s.store.GetCursor(accountID, scopeListCursorKey)
	if err != nil {
		return nil, err
	}

	log := logx.From(ctx)
	log.Debug("scope listing", "cursor_present", cursor != "")

	set, err := mailbox.SyncScopes(ctx, accountID, cursor)
	if errors.Is(err, provider.ErrCursorExpired) {
		log.Debug("scope listing decision", "decision", "cursor expired, relisting")
		set, err = mailbox.SyncScopes(ctx, accountID, "")
	}
	if err != nil {
		return nil, err
	}

	// The contract says Scopes is the complete current set, so anything in our
	// folder table that is missing from it has been deleted upstream.
	known, err := s.store.ListFolders(accountID)
	if err != nil {
		return nil, err
	}
	live := make(map[string]bool, len(set.Folders))
	for _, f := range set.Folders {
		live[f.ID] = true
		if err := s.store.UpsertFolder(f); err != nil {
			return nil, err
		}
	}
	deleted := 0
	for _, f := range known {
		if !live[f.ID] {
			if err := s.store.DeleteFolder(accountID, f.ID); err != nil {
				return nil, err
			}
			deleted++
		}
	}
	log.Debug("folders reconciled", "known", len(known), "live", len(set.Folders),
		"deleted", deleted, "cursor_out_present", set.Cursor != "")

	if set.Cursor != "" {
		if err := s.store.SetCursor(accountID, scopeListCursorKey, set.Cursor); err != nil {
			return nil, err
		}
	}
	return set.Scopes, nil
}

func (s *Syncer) syncScope(ctx context.Context, mailbox provider.Mailbox,
	accountID string, scope provider.Scope, firstRun bool) error {

	cursor, err := s.store.GetCursor(accountID, scope.ID)
	if err != nil {
		return err
	}
	initial := cursor == ""

	var since time.Time
	if s.opts.BackfillWindow > 0 {
		since = time.Now().Add(-s.opts.BackfillWindow)
	}

	log := logx.From(ctx).With("scope", scope.Name, "scope_id", scope.ID, "role", scope.Role)
	log.Debug("scope decision", "cursor_present", cursor != "", "initial", initial,
		"first_run", firstRun, "since", since)

	changes, err := mailbox.SyncMessages(ctx, accountID, scope, cursor, since)
	if errors.Is(err, provider.ErrCursorExpired) {
		log.Debug("scope decision", "decision", "cursor expired, resyncing scope")
		initial = true
		changes, err = mailbox.SyncMessages(ctx, accountID, scope, "", since)
	}
	if err != nil {
		return err
	}
	log.Debug("scope changes", "changed", len(changes.Changed), "removed", len(changes.Removed),
		"cursor_out_present", changes.Cursor != "")

	// Events are suppressed on an account's very first sync and on any forced
	// resync. Otherwise connecting an account would fire a mail_received for
	// every message already in it, which is noise rather than news.
	quiet := initial || firstRun

	for _, e := range changes.Changed {
		existed, err := s.store.EmailExists(accountID, e.ID)
		if err != nil {
			return err
		}
		if err := s.store.UpsertEmail(e); err != nil {
			return err
		}
		decision := "updated"
		switch {
		case quiet:
			decision = "suppressed"
		case !existed && scope.Role == "sentitems":
			decision = "new-sent"
		case !existed && !e.Draft:
			decision = "new"
		case !existed:
			decision = "new-draft"
		}
		// Body is counted, never quoted: the log says how much mail moved, not
		// what it said.
		log.Debug("message decision", "email_id", e.ID, "existed", existed, "draft", e.Draft,
			"has_attachments", e.HasAttachments, "body_bytes", len(e.Body), "decision", decision)
		if quiet {
			continue
		}
		email := e
		email.Role = scope.Role
		switch {
		case !existed && scope.Role == "sentitems":
			s.attachAttachments(ctx, mailbox, &email)
			s.emit(log, model.Event{Type: model.EventMailSent, AccountID: accountID, Email: &email})
		case !existed && !e.Draft:
			s.attachAttachments(ctx, mailbox, &email)
			s.emit(log, model.Event{Type: model.EventMailReceived, AccountID: accountID, Email: &email})
		case existed:
			s.emit(log, model.Event{Type: model.EventMailUpdated, AccountID: accountID, Email: &email})
		}
	}

	for _, id := range changes.Removed {
		if err := s.store.DeleteEmail(accountID, id); err != nil {
			return err
		}
		removal := "removed"
		if quiet {
			removal = "suppressed"
		}
		log.Debug("message decision", "email_id", id, "existed", true, "decision", removal)
		if !quiet {
			s.emit(log, model.Event{
				Type: model.EventMailDeleted, AccountID: accountID, EmailID: id,
			})
		}
	}

	if changes.Cursor != "" {
		return s.store.SetCursor(accountID, scope.ID, changes.Cursor)
	}
	return nil
}

// emit hands an event to the dispatcher and records that it left the syncer, so
// a missing webhook can be traced to either side of this boundary.
func (s *Syncer) emit(log *slog.Logger, ev model.Event) {
	emailID := ev.EmailID
	if ev.Email != nil {
		emailID = ev.Email.ID
	}
	log.Debug("event emitted", "event", ev.Type, "email_id", emailID)
	s.events.Emit(ev)
}

// attachAttachments fills in the attachment list for a new-mail event, so a
// subscriber can act on it without a second call. One provider round-trip
// per new message that has attachments; a failure degrades to the bare
// has_attachments flag rather than blocking the sync.
func (s *Syncer) attachAttachments(ctx context.Context, mailbox provider.Mailbox, e *model.Email) {
	if !e.HasAttachments || len(e.Attachments) > 0 {
		return
	}
	log := logx.From(ctx)
	log.Debug("attachment fetch", "email_id", e.ID)
	atts, err := mailbox.ListAttachments(ctx, e.AccountID, e.ID)
	if err != nil {
		log.Warn("listing attachments for event", "email_id", e.ID, "err", err)
		return
	}
	log.Debug("attachments fetched", "email_id", e.ID, "attachments", len(atts))
	e.Attachments = atts
}
