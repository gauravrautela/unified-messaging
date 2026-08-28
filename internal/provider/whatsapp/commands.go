package whatsapp

import (
	"context"
	"fmt"
	"time"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"

	"github.com/gauravrautela/unified-messaging/internal/logx"
	"github.com/gauravrautela/unified-messaging/internal/provider"
)

// Outbound commands. Every one of them resolves the live connection through
// connFor and reports provider.ErrNotConnected when the account has no live
// socket — that check happens before anything else, so a command against a
// disconnected account never touches the network. It is deliberately not
// ErrNotFound: the account and the message both exist, and the API turns this
// into 409 reconnect_required rather than a 404 that would read as
// "belongs to someone else".
//
// Logging follows the rule the rest of the package uses: chat ids only
// through logChatID (a group id is opaque and logged verbatim, a direct chat
// id is a phone number and goes through as a digest), message ids in full,
// and content — text, the phone passed to StartDirect, emoji — reduced to a
// byte count or dropped entirely. IsOnWhatsApp results are never logged with
// the phone that produced them.

// textMessage builds the message to send: a plain conversation, or an
// extended text message carrying a quote when quotedID is set. It is pure —
// no client, no I/O — which is what makes it directly testable.
func textMessage(text, quotedID string, quotedSender types.JID) *waE2E.Message {
	if quotedID == "" {
		return &waE2E.Message{Conversation: proto.String(text)}
	}
	return &waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{
		Text: proto.String(text),
		ContextInfo: &waE2E.ContextInfo{
			StanzaID:    proto.String(quotedID),
			Participant: proto.String(quotedSender.String()),
		},
	}}
}

// toMessageIDs converts the API's plain string ids to whatsmeow's typed ones.
// Pure, and separated out only so MarkRead's conversion is testable without a
// client.
func toMessageIDs(ids []string) []types.MessageID {
	out := make([]types.MessageID, len(ids))
	for i, id := range ids {
		out[i] = types.MessageID(id)
	}
	return out
}

// SendText sends a plain or quoted text message to a chat.
func (p *Provider) SendText(ctx context.Context, accountID, chatID, text, quotedID string) (provider.SendResult, error) {
	c := p.connFor(accountID)
	if c == nil {
		return provider.SendResult{}, provider.ErrNotConnected
	}
	chat, err := types.ParseJID(chatID)
	if err != nil {
		return provider.SendResult{}, fmt.Errorf("whatsapp: parse chat jid: %w", err)
	}
	resp, err := c.client.SendMessage(ctx, chat, textMessage(text, quotedID, chat))
	if err != nil {
		return provider.SendResult{}, err
	}
	logx.From(ctx).With("component", "whatsapp").Info("whatsapp send",
		"account_id", accountID, "chat_id", logChatID(chat), "message_id", string(resp.ID), "text_bytes", len(text))
	return provider.SendResult{MessageID: string(resp.ID)}, nil
}

// StartDirect resolves a phone number to the chat id for a direct
// conversation with it, checking first that the number is on WhatsApp at
// all. The phone itself is never logged, nor is the JID IsOnWhatsApp returns
// for it — only that a connection existed and whether the lookup succeeded.
func (p *Provider) StartDirect(ctx context.Context, accountID, phoneE164 string) (string, error) {
	c := p.connFor(accountID)
	if c == nil {
		return "", provider.ErrNotConnected
	}
	res, err := c.client.IsOnWhatsApp(ctx, []string{phoneE164})
	if err != nil {
		return "", err
	}
	if len(res) == 0 || !res[0].IsIn {
		return "", fmt.Errorf("%w: phone is not on WhatsApp", provider.ErrNotFound)
	}
	chat := res[0].JID.ToNonAD()
	logx.From(ctx).With("component", "whatsapp").Info("whatsapp start direct",
		"account_id", accountID, "chat_id", logChatID(chat))
	return chat.String(), nil
}

// selfJID returns the device's own JID, used as the sender identity when
// building a revoke or reaction on our own behalf. A nil Store.ID means the
// session's credentials are gone; only a relink can recover.
func (c *conn) selfJID() (types.JID, error) {
	if c.client.Store.ID == nil {
		return types.JID{}, provider.ErrReauthRequired
	}
	return *c.client.Store.ID, nil
}

// React sends a reaction (or, with an empty emoji, clears one) to a message.
func (p *Provider) React(ctx context.Context, accountID, chatID, messageID, emoji string) error {
	c := p.connFor(accountID)
	if c == nil {
		return provider.ErrNotConnected
	}
	chat, err := types.ParseJID(chatID)
	if err != nil {
		return fmt.Errorf("whatsapp: parse chat jid: %w", err)
	}
	self, err := c.selfJID()
	if err != nil {
		return err
	}
	_, err = c.client.SendMessage(ctx, chat, c.client.BuildReaction(chat, self, types.MessageID(messageID), emoji))
	if err != nil {
		return err
	}
	logx.From(ctx).With("component", "whatsapp").Info("whatsapp react",
		"account_id", accountID, "chat_id", logChatID(chat), "message_id", messageID, "emoji_bytes", len(emoji))
	return nil
}

// Edit replaces the text of a previously sent message.
func (p *Provider) Edit(ctx context.Context, accountID, chatID, messageID, text string) error {
	c := p.connFor(accountID)
	if c == nil {
		return provider.ErrNotConnected
	}
	chat, err := types.ParseJID(chatID)
	if err != nil {
		return fmt.Errorf("whatsapp: parse chat jid: %w", err)
	}
	_, err = c.client.SendMessage(ctx, chat, c.client.BuildEdit(chat, types.MessageID(messageID), textMessage(text, "", chat)))
	if err != nil {
		return err
	}
	logx.From(ctx).With("component", "whatsapp").Info("whatsapp edit",
		"account_id", accountID, "chat_id", logChatID(chat), "message_id", messageID, "text_bytes", len(text))
	return nil
}

// Delete revokes a previously sent message.
func (p *Provider) Delete(ctx context.Context, accountID, chatID, messageID string) error {
	c := p.connFor(accountID)
	if c == nil {
		return provider.ErrNotConnected
	}
	chat, err := types.ParseJID(chatID)
	if err != nil {
		return fmt.Errorf("whatsapp: parse chat jid: %w", err)
	}
	self, err := c.selfJID()
	if err != nil {
		return err
	}
	_, err = c.client.SendMessage(ctx, chat, c.client.BuildRevoke(chat, self, types.MessageID(messageID)))
	if err != nil {
		return err
	}
	logx.From(ctx).With("component", "whatsapp").Info("whatsapp delete",
		"account_id", accountID, "chat_id", logChatID(chat), "message_id", messageID)
	return nil
}

// MarkRead marks messages as read.
//
// whatsmeow's MarkRead wants the sender of the message being acknowledged,
// which matters for a group's read receipts. v1 passes the chat JID as the
// sender for every case. For a direct chat that is correct — chat and sender
// coincide. For a group it is not the participant who sent the message, so the
// receipt is attributed to the group JID itself; the chat is marked read, but
// per-sender attribution is lost. Getting that right would need the sender
// recorded alongside each stored message id, which is out of scope here.
//
// v1 also caps a mark-read call at the 50 most recent message ids (see the
// handler); the cap is documented in /docs §7.3.
func (p *Provider) MarkRead(ctx context.Context, accountID, chatID string, messageIDs []string) error {
	c := p.connFor(accountID)
	if c == nil {
		return provider.ErrNotConnected
	}
	chat, err := types.ParseJID(chatID)
	if err != nil {
		return fmt.Errorf("whatsapp: parse chat jid: %w", err)
	}
	if err := c.client.MarkRead(ctx, toMessageIDs(messageIDs), time.Now(), chat, chat); err != nil {
		return err
	}
	logx.From(ctx).With("component", "whatsapp").Info("whatsapp mark read",
		"account_id", accountID, "chat_id", logChatID(chat), "count", len(messageIDs))
	return nil
}

// Logout ends the WhatsApp session for good: the phone unlinks the device,
// and reconnecting afterwards needs a fresh link. An account with no live
// connection has nothing to log out of, which is not an error.
func (p *Provider) Logout(ctx context.Context, accountID string) error {
	c := p.connFor(accountID)
	if c == nil {
		return nil
	}
	if err := c.client.Logout(ctx); err != nil {
		return err
	}
	logx.From(ctx).With("component", "whatsapp").Info("whatsapp logout", "account_id", accountID)
	return nil
}
