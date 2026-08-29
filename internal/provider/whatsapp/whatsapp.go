// Package whatsapp is the WhatsApp adapter: the only place in this service
// that knows whatsmeow exists.
//
// It speaks the provider contracts and nothing else — it never touches the
// store, never fires webhooks and holds no policy. Inbound events go to the
// EventSink the runtime hands it at Connect; the runtime decides what to
// persist and what to publish. Device credentials live in whatsmeow's own
// tables inside our SQLite file, keyed by device JID.
package whatsapp

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"sync"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/store/sqlstore"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"

	"github.com/gauravrautela/unified-messaging/internal/model"
	"github.com/gauravrautela/unified-messaging/internal/provider"
)

// Name is the stable provider identifier recorded on accounts.
const Name = "WHATSAPP"

// Provider is the adapter. One instance serves every WhatsApp account in the
// process: the device container is shared, and each linked account gets its
// own client held in conns for the duration of its connection.
type Provider struct {
	container *sqlstore.Container
	log       *slog.Logger
	// RosterGroups is whether Chats scans joined groups. See
	// config.WhatsAppRosterGroups for why an operator would turn it off.
	RosterGroups bool

	mu    sync.Mutex
	conns map[string]*conn // accountID -> live connection (used by commands)
}

// Compile-time proof the adapter satisfies every contract the runtime uses.
var (
	_ provider.Provider = (*Provider)(nil)
	_ provider.Linker   = (*Provider)(nil)
	_ provider.Chatter  = (*Provider)(nil)
)

// New builds the adapter on an existing database handle. It shares the
// service's *sql.DB rather than opening its own so that whatsmeow's device
// tables live in the same database, under the same connection limits, as
// everything else.
//
// dialect is whatsmeow's own name for the engine behind db — "sqlite3" or
// "postgres" (see Store.Dialect) — never hardcoded here: whatsmeow's own
// migrations speak a different SQL dialect per engine, same as ours, and a
// mismatched dialect fails immediately (a SQLite PRAGMA sent to Postgres, or
// the reverse) rather than silently doing the wrong thing.
//
// deviceName is what the end user sees in WhatsApp's "linked devices" list.
func New(db *sql.DB, dialect, deviceName string, log *slog.Logger) (*Provider, error) {
	if log == nil {
		log = slog.Default()
	}
	// whatsmeow's own logging is silenced: this adapter logs the events that
	// matter through slog, with the redaction rules the rest of the service uses.
	c := sqlstore.NewWithDB(db, dialect, waLog.Noop)
	if err := c.Upgrade(context.Background()); err != nil {
		return nil, fmt.Errorf("whatsapp: store upgrade: %w", err)
	}
	if deviceName != "" {
		store.DeviceProps.Os = proto.String(deviceName)
	}
	return &Provider{
		container:    c,
		log:          log.With("component", "whatsapp"),
		conns:        map[string]*conn{},
		RosterGroups: true,
	}, nil
}

func (p *Provider) Name() string                 { return Name }
func (p *Provider) Kind() string                 { return model.AccountKindChat }
func (p *Provider) Auth() provider.Authenticator { return nil }
func (p *Provider) Mailbox() provider.Mailbox    { return nil }
func (p *Provider) Push() provider.Pusher        { return nil }
func (p *Provider) Linker() provider.Linker      { return p }
func (p *Provider) Chat() provider.Chatter       { return p }

// newClient builds a whatsmeow client for one device.
//
// Reconnection is deliberately the caller's job: the chat runtime owns the
// backoff policy and builds a fresh client for each attempt, so whatsmeow's own
// auto-reconnect (on by default in NewClient) would run a second, invisible
// connection alongside the one the runtime is managing.
func (p *Provider) newClient(device *store.Device) *whatsmeow.Client {
	c := whatsmeow.NewClient(device, waLogger(p.log))
	c.EnableAutoReconnect = false
	// The phone pushes message-history blobs to a linked device; this service
	// mirrors from live events only and never reads them. Downloading them is
	// not free either: whatsmeow stores every LID mapping a blob mentions, one
	// statement at a time, under the lock every inbound decrypt takes — on a
	// remote database that stalls live messages for minutes per blob.
	c.ManualHistorySyncDownload = true
	return c
}

// newPairingClient is newClient for the one client that performs a QR pairing.
//
// WhatsApp always sends a 515 stream error immediately after pair-success, and
// whatsmeow's handler for it reconnects in the background unless
// DisableLoginAutoReconnect is set — EnableAutoReconnect does not gate it. That
// reconnect runs on the library's own background context, which the link
// session's cancel cannot reach, so it would leave a second live socket for the
// device the chat runtime is about to Attach: StreamReplaced flapping right
// after a successful link, plus a goroutine nothing ever closes.
func (p *Provider) newPairingClient(device *store.Device) *whatsmeow.Client {
	c := p.newClient(device)
	c.DisableLoginAutoReconnect = true
	return c
}

// connFor returns the live connection for an account, or nil when the account
// is not currently connected.
func (p *Provider) connFor(accountID string) *conn {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.conns[accountID]
}
