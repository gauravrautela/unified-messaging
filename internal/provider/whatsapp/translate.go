package whatsapp

import (
	"strings"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"

	"github.com/gauravrautela/unified-messaging/internal/model"
)

// This file is pure translation: WhatsApp wire shapes in, our model out. It
// touches no client, no store and no network, which is what makes the inbound
// path testable without a phone.

func chatKind(jid types.JID) string {
	if jid.Server == types.GroupServer {
		return "group"
	}
	return "direct"
}

// attendeeFrom builds the API identity for a WhatsApp user. Phone JIDs are the
// stable public id; a LID (privacy id) is used only when no phone is known.
func attendeeFrom(jid, alt types.JID, pushName string) model.Attendee {
	pick := jid
	if jid.Server == types.HiddenUserServer && alt.Server == types.DefaultUserServer {
		pick = alt
	}
	a := model.Attendee{ID: pick.ToNonAD().String(), Name: pushName}
	if pick.Server == types.DefaultUserServer {
		a.Phone = "+" + pick.User
	}
	return a
}

// messageFrom classifies an inbound event: kind is one of
// message | reaction | revoke | edit. For reaction/revoke/edit the target id
// is returned in QuotedMessageID and the new text/emoji in Text.
func messageFrom(e *events.Message) (model.ChatMessage, string) {
	m := model.ChatMessage{
		ID: e.Info.ID, ChatID: e.Info.Chat.ToNonAD().String(), IsFromMe: e.Info.IsFromMe,
		Sender: attendeeFrom(e.Info.Sender, e.Info.SenderAlt, e.Info.PushName),
		SentAt: e.Info.Timestamp.UTC(), Kind: "text", Reactions: []model.Reaction{},
	}
	msg := e.Message
	switch {
	case msg.GetReactionMessage() != nil:
		m.QuotedMessageID = msg.GetReactionMessage().GetKey().GetID()
		m.Text = msg.GetReactionMessage().GetText()
		return m, "reaction"
	case msg.GetProtocolMessage() != nil:
		p := msg.GetProtocolMessage()
		m.QuotedMessageID = p.GetKey().GetID()
		switch p.GetType() {
		case waE2E.ProtocolMessage_REVOKE:
			return m, "revoke"
		case waE2E.ProtocolMessage_MESSAGE_EDIT:
			m.Text = textOf(p.GetEditedMessage())
			return m, "edit"
		}
		// App-state syncs, key requests and friends: nothing a chat log shows.
		m.Kind = "unsupported"
		m.Text = "[protocol]"
		return m, "message"
	case msg.GetConversation() != "":
		m.Text = msg.GetConversation()
	case msg.GetExtendedTextMessage() != nil:
		m.Text = msg.GetExtendedTextMessage().GetText()
		m.QuotedMessageID = msg.GetExtendedTextMessage().GetContextInfo().GetStanzaID()
	default:
		m.Kind = "unsupported"
		m.Text = mediaLabel(msg)
	}
	return m, "message"
}

// textOf pulls the body out of a nested message (an edit's replacement).
func textOf(msg *waE2E.Message) string {
	if msg.GetConversation() != "" {
		return msg.GetConversation()
	}
	return msg.GetExtendedTextMessage().GetText()
}

// mediaLabel is the placeholder shown for content this service does not carry.
// Media download is deliberately out of scope, so the API reports the shape of
// the message rather than pretending it was empty.
func mediaLabel(msg *waE2E.Message) string {
	switch {
	case msg.GetImageMessage() != nil:
		return "[image]"
	case msg.GetVideoMessage() != nil:
		return "[video]"
	case msg.GetAudioMessage() != nil:
		return "[audio]"
	case msg.GetDocumentMessage() != nil:
		return "[document]"
	case msg.GetStickerMessage() != nil:
		return "[sticker]"
	case msg.GetLocationMessage() != nil, msg.GetLiveLocationMessage() != nil:
		return "[location]"
	case msg.GetContactMessage() != nil, msg.GetContactsArrayMessage() != nil:
		return "[contact]"
	case msg.GetPollCreationMessage() != nil:
		return "[poll]"
	default:
		return "[unsupported]"
	}
}

// receiptStatus maps a WhatsApp receipt to the status we record, reporting
// false for receipt types the message log has no use for.
func receiptStatus(t types.ReceiptType) (string, bool) {
	switch t {
	case types.ReceiptTypeDelivered:
		return "delivered", true
	case types.ReceiptTypeRead, types.ReceiptTypeReadSelf:
		return "read", true
	}
	return "", false
}

// phoneToJID turns an E.164 number into the JID a direct chat is addressed by.
func phoneToJID(e164 string) types.JID {
	return types.NewJID(strings.TrimPrefix(strings.TrimSpace(e164), "+"), types.DefaultUserServer)
}

// firstNonEmpty returns the first non-blank string, used to pick the best of
// several name fields a contact may carry.
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
