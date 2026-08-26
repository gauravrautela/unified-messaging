// Package provider defines what a messaging backend must do to be usable by
// this service, and nothing about how any particular one does it.
//
// The core — store, syncer, events, api — is written entirely against these
// contracts. Outlook is the first implementation; a second one should require
// no changes above this line.
package provider

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"time"

	"github.com/gauravrautela/unified-messaging/internal/model"
)

// Sentinel conditions every provider must be able to report, because the core
// changes behaviour based on them.
var (
	// ErrReauthRequired means the stored grant is dead and only the end user can
	// fix it, by walking the connect flow again. No retry will help.
	ErrReauthRequired = errors.New("provider: re-consent required")

	// ErrCursorExpired means an incremental sync cursor is no longer usable and
	// the scope must be resynchronized from scratch.
	ErrCursorExpired = errors.New("provider: sync cursor expired")

	// ErrNotFound is a missing message, folder, or subscription.
	ErrNotFound = errors.New("provider: not found")

	// ErrSubscriptionExists means an equivalent push subscription is already
	// registered upstream, typically because our local record was lost.
	ErrSubscriptionExists = errors.New("provider: subscription already exists")

	// ErrLinkTimeout means a link session's pairing window elapsed before the
	// end user scanned the code.
	ErrLinkTimeout = errors.New("provider: link timed out")

	// ErrLinkCancelled means the end user (or the caller) cancelled an in-flight
	// link session.
	ErrLinkCancelled = errors.New("provider: link cancelled")
)

// Token is an OAuth grant, stripped of provider specifics.
type Token struct {
	AccessToken  string
	RefreshToken string
	Scope        string
	ExpiresAt    time.Time
}

// Identity is who a freshly connected grant belongs to.
type Identity struct {
	Identifier string
	Email      string
	Name       string
}

// TokenSource hands out a valid access token for a connected account.
// internal/accounts implements it; providers consume it.
type TokenSource interface {
	AccessToken(ctx context.Context, accountID string, force bool) (string, error)
}

// Authenticator covers attaching an account and keeping its grant alive.
type Authenticator interface {
	// AuthorizeURL is where the end user is sent to grant consent.
	AuthorizeURL(state, challenge string, forceConsent bool) string
	Exchange(ctx context.Context, code, verifier string) (Token, error)
	Refresh(ctx context.Context, refreshToken string) (Token, error)
	// Identify reports which mailbox an access token belongs to. It takes a bare
	// token because at connect time no account record exists yet.
	Identify(ctx context.Context, accessToken string) (Identity, error)
}

// Scope is one unit of incremental synchronization.
//
// This is the abstraction that lets the sync loop stay provider-agnostic.
// Microsoft Graph only exposes message delta per mail folder, so the Outlook
// provider returns one scope per folder. A provider with a single mailbox-wide
// cursor (Gmail's historyId, an IMAP MODSEQ) returns exactly one scope. The
// core never needs to know which it is dealing with.
type Scope struct {
	ID   string
	Name string
	// Role is a well-known mailbox role when the scope maps to one:
	// "inbox", "sentitems", "drafts", "deleteditems", "archive", "junkemail".
	Role string
}

// ScopeSet is the result of listing scopes, along with any folder changes
// discovered in the process.
type ScopeSet struct {
	Scopes []Scope
	// Folders and RemovedFolders let the core keep its folder table current for
	// providers that have folders. Both may be empty.
	Folders        []model.Folder
	RemovedFolders []string
	// Cursor resumes the next scope listing. Empty means the provider does not
	// support incremental scope discovery and always returns the full set.
	Cursor string
}

// Changes is one completed round of message synchronization for a scope.
type Changes struct {
	Changed []model.Email
	// Removed holds message IDs that left the scope. Providers generally cannot
	// distinguish "deleted" from "moved elsewhere", so a move appears as a
	// removal here and a creation in the destination scope.
	Removed []string
	// Cursor resumes the next round. Empty means the round did not complete.
	Cursor string
}

// MessageUpdate is the set of mutable flags the core exposes. Keeping it a
// struct rather than a provider-shaped patch map is what stops Graph's wire
// format leaking into the HTTP layer.
type MessageUpdate struct {
	Read    *bool
	Flagged *bool
}

// SendResult reports what a send produced. MessageID is empty for providers
// that do not return one — Graph's /sendMail, for instance.
type SendResult struct {
	MessageID string
}

// Mailbox is reading and writing mail for one connected account.
type Mailbox interface {
	// SyncScopes lists the units of incremental sync, reporting folder changes
	// along the way. An empty cursor means "start from the beginning".
	SyncScopes(ctx context.Context, accountID, cursor string) (ScopeSet, error)

	// SyncMessages walks one scope's changes to completion. An empty cursor
	// performs the initial backfill, bounded by since when it is non-zero;
	// otherwise since is ignored because the cursor already encodes it.
	// Implementations must return ErrCursorExpired when a cursor goes stale.
	SyncMessages(ctx context.Context, accountID string, scope Scope, cursor string, since time.Time) (Changes, error)

	GetMessage(ctx context.Context, accountID, messageID string) (model.Email, error)
	UpdateMessage(ctx context.Context, accountID, messageID string, upd MessageUpdate) error

	ListAttachments(ctx context.Context, accountID, messageID string) ([]model.Attachment, error)
	DownloadAttachment(ctx context.Context, accountID, messageID, attachmentID string) ([]byte, error)

	Send(ctx context.Context, accountID string, req model.SendRequest) (SendResult, error)
	// Reply must thread correctly. Providers that generate threading headers
	// themselves should use that path rather than composing a new message.
	Reply(ctx context.Context, accountID, messageID string, req model.SendRequest) (SendResult, error)
	Forward(ctx context.Context, accountID, messageID string, req model.SendRequest) (SendResult, error)

	CreateDraft(ctx context.Context, accountID string, req model.SendRequest) (model.Email, error)
	SendDraft(ctx context.Context, accountID, draftID string) error
}

// Subscription is a live push registration with a provider.
type Subscription struct {
	ID string
	// Resource identifies what is being watched, in provider terms.
	Resource string
	// ClientState is a shared secret the provider echoes back, used to
	// authenticate inbound notifications.
	ClientState string
	ExpiresAt   time.Time
}

// PushConfig is what a provider needs to start delivering notifications.
type PushConfig struct {
	NotificationURL string
	LifecycleURL    string
	ClientState     string
}

// Notification is one inbound push event, normalized.
//
// It carries no message data on purpose. A notification tells the core that
// something changed; the incremental sync is what determines what. That keeps
// push and polling on a single code path with one set of dedupe rules.
type Notification struct {
	SubscriptionID string
	ClientState    string
	// Lifecycle is set when the provider is warning about the subscription
	// itself rather than reporting a data change.
	Lifecycle LifecycleAction
}

// LifecycleAction is what the core should do about a subscription warning.
type LifecycleAction int

const (
	// LifecycleNone is an ordinary data-change notification.
	LifecycleNone LifecycleAction = iota
	// LifecycleReauthorize asks us to prove the grant is still valid, then renew.
	LifecycleReauthorize
	// LifecycleRecreate means the subscription is gone or dropped events, so it
	// must be rebuilt and the account resynchronized.
	LifecycleRecreate
)

// Pusher is optional. Providers without a push mechanism — IMAP, for instance —
// simply do not implement it, and the core falls back to polling.
type Pusher interface {
	Create(ctx context.Context, accountID string, cfg PushConfig) (Subscription, error)
	Renew(ctx context.Context, accountID, subscriptionID string) (Subscription, error)
	Delete(ctx context.Context, accountID, subscriptionID string) error
	List(ctx context.Context, accountID string) ([]Subscription, error)

	// RenewBefore is how much remaining lifetime should trigger a renewal.
	RenewBefore() time.Duration

	// ParseNotifications decodes an inbound push payload. Returning an empty
	// slice with no error means the payload was valid but carried nothing to do.
	ParseNotifications(raw []byte) ([]Notification, error)

	// ValidationResponse handles a provider's endpoint-validation challenge.
	//
	// Most push systems refuse to register an endpoint until it proves it is
	// listening, usually by echoing a token back. Returning ok=false means the
	// request is an ordinary notification and should be parsed instead.
	ValidationResponse(query url.Values) (body string, ok bool)
}

// Provider is one backend. Capabilities are optional: a mail provider returns
// nil from Linker() and Chat(); a chat provider returns nil from Auth(),
// Mailbox() and Push(). Callers test capabilities, never names.
type Provider interface {
	// Name is the stable identifier stored on accounts, e.g. "OUTLOOK".
	Name() string
	Kind() string // model.AccountKindMail | model.AccountKindChat
	Auth() Authenticator
	Linker() Linker
	Mailbox() Mailbox
	Chat() Chatter
	// Push returns nil when this provider cannot deliver push notifications.
	Push() Pusher
}

// ---- chat providers (linked device, live socket) ----

type LinkCode struct {
	Code      string
	ExpiresAt time.Time
}

type LinkResult struct {
	Identity  Identity
	DeviceJID string
	Err       error
}

// LinkSession is one pairing attempt. Codes stream until paired, expired or
// closed; Result resolves exactly once.
type LinkSession interface {
	Codes() <-chan LinkCode
	Result() <-chan LinkResult
	Close()
}

type Linker interface {
	StartLink(ctx context.Context) (LinkSession, error)
}

// EventSink receives domain events from a live connection. Implementations
// must be safe for concurrent use; the adapter calls them from its own
// goroutines.
type EventSink interface {
	Message(accountID string, m model.ChatMessage, chat model.Chat, sender model.Attendee)
	Receipt(accountID, chatID string, messageIDs []string, status string) // delivered | read
	Reaction(accountID, chatID, messageID string, r model.Reaction)
	Edited(accountID, chatID, messageID, text string, at time.Time)
	Deleted(accountID, chatID, messageID string)
	Disconnected(accountID, reason string, loggedOut bool)
}

type ChatConn interface {
	Close() error
}

type Chatter interface {
	Connect(ctx context.Context, accountID, deviceJID string, sink EventSink) (ChatConn, error)
	Forget(ctx context.Context, deviceJID string) error
	Chats(ctx context.Context, accountID string) ([]model.Chat, []model.Attendee, []model.ChatMember, error)
	SendText(ctx context.Context, accountID, chatID, text, quotedID string) (SendResult, error)
	StartDirect(ctx context.Context, accountID, phoneE164 string) (chatID string, err error)
	React(ctx context.Context, accountID, chatID, messageID, emoji string) error
	Edit(ctx context.Context, accountID, chatID, messageID, text string) error
	Delete(ctx context.Context, accountID, chatID, messageID string) error
	MarkRead(ctx context.Context, accountID, chatID string, messageIDs []string) error
	Logout(ctx context.Context, accountID string) error
}

// Registry resolves a provider by the name recorded on an account.
type Registry struct {
	byName map[string]Provider
}

func NewRegistry(ps ...Provider) *Registry {
	r := &Registry{byName: make(map[string]Provider, len(ps))}
	for _, p := range ps {
		r.byName[p.Name()] = p
	}
	return r
}

// Add registers an additional provider after construction, for a backend
// whose wiring (e.g. reading config, opening its own store handle) is
// conditional and so cannot always go through the NewRegistry(...) call at
// startup. Only ever called during startup, before the registry is served
// to any request handler, so — like the rest of this type — it takes no
// lock.
func (r *Registry) Add(p Provider) {
	r.byName[p.Name()] = p
}

func (r *Registry) Get(name string) (Provider, error) {
	p, ok := r.byName[name]
	if !ok {
		return nil, fmt.Errorf("%w: provider %q is not registered", ErrNotFound, name)
	}
	return p, nil
}

// Names lists registered providers in a stable order.
func (r *Registry) Names() []string {
	out := make([]string, 0, len(r.byName))
	for n := range r.byName {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Default is the provider to use when a caller does not name one. It is only
// unambiguous while exactly one provider is registered.
func (r *Registry) Default() (Provider, error) {
	if len(r.byName) != 1 {
		return nil, errors.New("provider: several providers registered; specify one explicitly")
	}
	for _, p := range r.byName {
		return p, nil
	}
	return nil, errors.New("provider: none registered")
}
