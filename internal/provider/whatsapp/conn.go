package whatsapp

import (
	"context"
	"fmt"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"

	"github.com/gauravrautela/unified-messaging/internal/logx"
	"github.com/gauravrautela/unified-messaging/internal/model"
	"github.com/gauravrautela/unified-messaging/internal/provider"
)

// conn is one account's live socket. Commands (task 5) reach the client
// through the provider's conns map; inbound events leave through the sink.
type conn struct {
	p         *Provider
	accountID string
	client    *whatsmeow.Client
	sink      provider.EventSink
}

var _ provider.ChatConn = (*conn)(nil)

// Connect opens the socket for an already-linked device.
//
// A missing device row means the credentials this account was built on are
// gone — nothing but a fresh link can fix that, so it reports
// ErrReauthRequired rather than an error the runtime would retry forever.
func (p *Provider) Connect(ctx context.Context, accountID, deviceJID string, sink provider.EventSink) (provider.ChatConn, error) {
	log := logx.From(ctx).With("component", "whatsapp")

	jid, err := types.ParseJID(deviceJID)
	if err != nil {
		return nil, fmt.Errorf("whatsapp: parse device jid: %w", err)
	}
	device, err := p.container.GetDevice(ctx, jid)
	if err != nil {
		return nil, fmt.Errorf("whatsapp: load device: %w", err)
	}
	if device == nil {
		log.Warn("whatsapp device row missing; relink required")
		return nil, provider.ErrReauthRequired
	}

	c := &conn{p: p, accountID: accountID, client: p.newClient(device), sink: sink}
	// Registered before Connect so that events arriving during the initial
	// handshake — history, receipts, an immediate logout — are not dropped.
	c.client.AddEventHandler(c.handle)
	if err := c.client.Connect(); err != nil {
		return nil, fmt.Errorf("whatsapp: connect: %w", err)
	}

	p.mu.Lock()
	if prev := p.conns[accountID]; prev != nil && prev != c {
		// Defensive: a second Connect for the same account replaces the first.
		go prev.client.Disconnect()
	}
	p.conns[accountID] = c
	p.mu.Unlock()

	log.Info("whatsapp connected")
	return c, nil
}

// Close drops the socket and forgets the connection. Credentials stay put:
// this is a disconnect, not an unlink.
func (c *conn) Close() error {
	c.p.mu.Lock()
	if c.p.conns[c.accountID] == c {
		delete(c.p.conns, c.accountID)
	}
	c.p.mu.Unlock()
	c.client.Disconnect()
	c.p.log.Info("whatsapp disconnected", "account_id", c.accountID)
	return nil
}

// handle translates whatsmeow events into sink calls. It runs on whatsmeow's
// goroutines, so it does only translation and publishing — no I/O, no store.
func (c *conn) handle(evt any) {
	switch v := evt.(type) {
	case *events.Message:
		m, kind := messageFrom(v)
		m.AccountID = c.accountID
		c.p.log.Debug("whatsapp inbound",
			"account_id", c.accountID, "chat_id", logChatID(v.Info.Chat), "message_id", m.ID,
			"kind", kind, "text_bytes", len(m.Text))
		switch kind {
		case "message":
			chat := model.Chat{AccountID: c.accountID, ID: m.ChatID, Kind: chatKind(v.Info.Chat)}
			if chat.Kind == "direct" && !m.IsFromMe {
				chat.Name = m.Sender.Name
			}
			c.sink.Message(c.accountID, m, chat, m.Sender)
		case "reaction":
			c.sink.Reaction(c.accountID, m.ChatID, m.QuotedMessageID,
				model.Reaction{AttendeeID: m.Sender.ID, Emoji: m.Text, At: m.SentAt})
		case "revoke":
			c.sink.Deleted(c.accountID, m.ChatID, m.QuotedMessageID)
		case "edit":
			c.sink.Edited(c.accountID, m.ChatID, m.QuotedMessageID, m.Text, m.SentAt)
		}
	case *events.Receipt:
		st, ok := receiptStatus(v.Type)
		if !ok {
			return
		}
		ids := make([]string, len(v.MessageIDs))
		for i, id := range v.MessageIDs {
			ids[i] = string(id)
		}
		c.p.log.Debug("whatsapp receipt", "account_id", c.accountID,
			"chat_id", logChatID(v.Chat), "kind", st, "count", len(ids))
		c.sink.Receipt(c.accountID, v.Chat.ToNonAD().String(), ids, st)
	case *events.LoggedOut:
		// The device was removed from the phone: only a relink can recover.
		c.p.log.Warn("whatsapp logged out", "account_id", c.accountID, "kind", v.Reason.String())
		c.sink.Disconnected(c.accountID, "logged out: "+v.Reason.String(), true)
	case *events.TemporaryBan:
		c.p.log.Warn("whatsapp temporarily banned", "account_id", c.accountID, "kind", v.Code.String())
		c.sink.Disconnected(c.accountID, "temporary ban: "+v.Code.String(), true)
	case *events.StreamReplaced:
		c.p.log.Warn("whatsapp stream replaced", "account_id", c.accountID)
		c.sink.Disconnected(c.accountID, "stream replaced", false)
	// A rejected connection never produces events.Disconnected: whatsmeow calls
	// expectDisconnect() before dispatching these, which suppresses it. Without
	// them the runtime would keep believing a dead socket is connected.
	case *events.ConnectFailure:
		c.p.log.Warn("whatsapp connect failure", "account_id", c.accountID, "kind", v.Reason.String())
		c.sink.Disconnected(c.accountID, "connect failure: "+v.Reason.String(), false)
	case *events.ClientOutdated:
		c.p.log.Error("whatsapp client outdated", "account_id", c.accountID)
		c.sink.Disconnected(c.accountID, "client outdated", false)
	case *events.CATRefreshError:
		reason := "cat refresh error"
		if v.Error != nil {
			reason += ": " + v.Error.Error()
		}
		c.p.log.Warn("whatsapp cat refresh error", "account_id", c.accountID, "error", reason)
		c.sink.Disconnected(c.accountID, reason, false)
	case *events.Disconnected:
		c.p.log.Info("whatsapp socket closed", "account_id", c.accountID)
		c.sink.Disconnected(c.accountID, "disconnected", false)
	}
}

// Forget deletes a device's credentials locally. It does not tell WhatsApp —
// Logout does that — so it is the right call once the grant is already dead.
func (p *Provider) Forget(ctx context.Context, deviceJID string) error {
	log := logx.From(ctx).With("component", "whatsapp")
	jid, err := types.ParseJID(deviceJID)
	if err != nil {
		return fmt.Errorf("whatsapp: parse device jid: %w", err)
	}
	device, err := p.container.GetDevice(ctx, jid)
	if err != nil {
		return fmt.Errorf("whatsapp: load device: %w", err)
	}
	if device == nil {
		return nil // already gone: forgetting twice is not an error
	}
	if err := p.container.DeleteDevice(ctx, device); err != nil {
		return fmt.Errorf("whatsapp: delete device: %w", err)
	}
	log.Info("whatsapp device forgotten")
	return nil
}

// Chats returns the roster we can know without message history: the groups
// this device has joined, with their members, plus known contacts as direct
// chats. Names come from the contact store; a phone number is only exposed
// for phone JIDs, never for a privacy id.
func (p *Provider) Chats(ctx context.Context, accountID string) ([]model.Chat, []model.Attendee, []model.ChatMember, error) {
	log := logx.From(ctx).With("component", "whatsapp")

	c := p.connFor(accountID)
	if c == nil {
		return nil, nil, nil, provider.ErrNotFound
	}

	groups, err := c.client.GetJoinedGroups(ctx)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("whatsapp: joined groups: %w", err)
	}

	var chats []model.Chat
	var members []model.ChatMember
	seen := map[string]model.Attendee{}

	for _, g := range groups {
		chatID := g.JID.ToNonAD().String()
		chats = append(chats, model.Chat{AccountID: accountID, ID: chatID, Kind: "group", Name: g.Name})
		for _, part := range g.Participants {
			a := attendeeFrom(part.JID, part.PhoneNumber, "")
			if info, err := c.client.Store.Contacts.GetContact(ctx, part.JID); err == nil && info.Found {
				a.Name = firstNonEmpty(info.FullName, info.PushName, info.BusinessName)
			}
			rememberAttendee(seen, a)
			role := ""
			if part.IsAdmin || part.IsSuperAdmin {
				role = "admin"
			}
			members = append(members, model.ChatMember{ChatID: chatID, AttendeeID: a.ID, Role: role})
		}
	}

	contacts, err := c.client.Store.Contacts.GetAllContacts(ctx)
	if err != nil {
		log.Warn("whatsapp: reading contacts", "error", err.Error())
	}
	for jid, info := range contacts {
		if jid.Server != types.DefaultUserServer {
			continue // a LID-only contact has no chat we can address by number
		}
		a := attendeeFrom(jid, types.JID{}, firstNonEmpty(info.FullName, info.PushName, info.BusinessName))
		rememberAttendee(seen, a)
		chats = append(chats, model.Chat{AccountID: accountID, ID: a.ID, Kind: "direct", Name: a.Name})
	}

	if id := c.client.Store.ID; id != nil {
		self := attendeeFrom(*id, types.JID{}, c.client.Store.PushName)
		self.IsSelf = true
		seen[self.ID] = self
	}

	attendees := make([]model.Attendee, 0, len(seen))
	for _, a := range seen {
		attendees = append(attendees, a)
	}
	log.Info("whatsapp roster read", "chats", len(chats), "attendees", len(attendees), "members", len(members))
	return chats, attendees, members, nil
}
