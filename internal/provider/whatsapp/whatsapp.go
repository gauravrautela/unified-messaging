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
	container  *sqlstore.Container
	deviceName string
	log        *slog.Logger

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
// tables live in the same file, under the same connection limits, as
// everything else.
//
// deviceName is what the end user sees in WhatsApp's "linked devices" list.
func New(db *sql.DB, deviceName string, log *slog.Logger) (*Provider, error) {
	if log == nil {
		log = slog.Default()
	}
	// whatsmeow's own logging is silenced: this adapter logs the events that
	// matter through slog, with the redaction rules the rest of the service uses.
	c := sqlstore.NewWithDB(db, "sqlite3", waLog.Noop)
	if err := c.Upgrade(context.Background()); err != nil {
		return nil, fmt.Errorf("whatsapp: store upgrade: %w", err)
	}
	if deviceName != "" {
		store.DeviceProps.Os = proto.String(deviceName)
	}
	return &Provider{
		container:  c,
		deviceName: deviceName,
		log:        log.With("component", "whatsapp"),
		conns:      map[string]*conn{},
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
	c := whatsmeow.NewClient(device, waLog.Noop)
	c.EnableAutoReconnect = false
	return c
}

// connFor returns the live connection for an account, or nil when the account
// is not currently connected.
func (p *Provider) connFor(accountID string) *conn {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.conns[accountID]
}
