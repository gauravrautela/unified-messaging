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
	"sync"
	"time"

	"github.com/gauravrautela/unified-messaging/internal/accounts"
	"github.com/gauravrautela/unified-messaging/internal/events"
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
			for _, a := range accts {
				if a.Status == model.AccountOK {
					s.Wake(a.ID)
				}
			}
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
	acct, err := s.store.GetAnyAccount(accountID)
	if err != nil {
		return err
	}
	if acct.Status != model.AccountOK {
		s.log.Debug("skipping sync for non-OK account",
			"account_id", accountID, "status", acct.Status)
		return nil
	}
	mailbox, err := s.mailboxFor(acct)
	if err != nil {
		return err
	}
	firstRun := acct.LastSyncedAt == nil

	scopes, err := s.syncScopes(ctx, mailbox, accountID)
	if err != nil {
		if errors.Is(err, provider.ErrReauthRequired) {
			return nil // status already flipped; nothing more to do
		}
		return err
	}

	for _, scope := range scopes {
		if err := s.syncScope(ctx, mailbox, accountID, scope, firstRun); err != nil {
			if errors.Is(err, provider.ErrReauthRequired) {
				return nil
			}
			// One bad scope should not stop the rest of the account.
			s.log.Error("scope sync failed",
				"account_id", accountID, "scope", scope.Name, "err", err)
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

	set, err := mailbox.SyncScopes(ctx, accountID, cursor)
	if errors.Is(err, provider.ErrCursorExpired) {
		s.log.Info("scope cursor expired, relisting", "account_id", accountID)
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
	for _, f := range known {
		if !live[f.ID] {
			if err := s.store.DeleteFolder(accountID, f.ID); err != nil {
				return nil, err
			}
		}
	}

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

	changes, err := mailbox.SyncMessages(ctx, accountID, scope, cursor, since)
	if errors.Is(err, provider.ErrCursorExpired) {
		s.log.Info("sync cursor expired, resyncing scope",
			"account_id", accountID, "scope", scope.Name)
		initial = true
		changes, err = mailbox.SyncMessages(ctx, accountID, scope, "", since)
	}
	if err != nil {
		return err
	}

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
		if quiet {
			continue
		}
		email := e
		email.Role = scope.Role
		switch {
		case !existed && scope.Role == "sentitems":
			s.attachAttachments(ctx, mailbox, &email)
			s.events.Emit(model.Event{Type: model.EventMailSent, AccountID: accountID, Email: &email})
		case !existed && !e.Draft:
			s.attachAttachments(ctx, mailbox, &email)
			s.events.Emit(model.Event{Type: model.EventMailReceived, AccountID: accountID, Email: &email})
		case existed:
			s.events.Emit(model.Event{Type: model.EventMailUpdated, AccountID: accountID, Email: &email})
		}
	}

	for _, id := range changes.Removed {
		if err := s.store.DeleteEmail(accountID, id); err != nil {
			return err
		}
		if !quiet {
			s.events.Emit(model.Event{
				Type: model.EventMailDeleted, AccountID: accountID, EmailID: id,
			})
		}
	}

	if changes.Cursor != "" {
		return s.store.SetCursor(accountID, scope.ID, changes.Cursor)
	}
	return nil
}

// attachAttachments fills in the attachment list for a new-mail event, so a
// subscriber can act on it without a second call. One provider round-trip
// per new message that has attachments; a failure degrades to the bare
// has_attachments flag rather than blocking the sync.
func (s *Syncer) attachAttachments(ctx context.Context, mailbox provider.Mailbox, e *model.Email) {
	if !e.HasAttachments || len(e.Attachments) > 0 {
		return
	}
	atts, err := mailbox.ListAttachments(ctx, e.AccountID, e.ID)
	if err != nil {
		s.log.Warn("listing attachments for event", "account_id", e.AccountID, "email_id", e.ID, "err", err)
		return
	}
	e.Attachments = atts
}
