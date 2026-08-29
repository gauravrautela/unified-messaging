package whatsapp

import (
	"fmt"
	"strings"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/gauravrautela/unified-messaging/internal/logx"
	"github.com/gauravrautela/unified-messaging/internal/model"
)

// This file is pure translation: WhatsApp wire shapes in, our model out. It
// touches no client, no store and no network, which is what makes the inbound
// path testable without a phone.

func chatKind(jid types.JID) string {
	switch {
	case jid.Server == types.GroupServer:
		return "group"
	case jid.Server == types.NewsletterServer:
		return "channel"
	case jid.Server == types.BroadcastServer:
		// status@broadcast is a Status post; any other broadcast list is a
		// one-to-many send from the phone, which readers still think of as
		// a status-like feed rather than a conversation.
		return "status"
	}
	return "direct"
}

// rememberAttendee records a into seen, protecting a name or phone that has
// already been resolved: the roster is assembled from several passes over the
// same people (each group's participants, then the contact list), and a later
// pass routinely has no name for a contact the first one resolved through the
// contact store, or no phone for a participant exposed only as a privacy id.
// Anything else about the newer record wins.
func rememberAttendee(seen map[string]model.Attendee, a model.Attendee) {
	if prev, ok := seen[a.ID]; ok {
		if a.Name == "" {
			a.Name = prev.Name
		}
		if a.Phone == "" {
			a.Phone = prev.Phone
		}
	}
	seen[a.ID] = a
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

// logChatID renders a chat id safe to log. A group JID is an opaque server id
// and goes through verbatim; a direct chat's JID *is* the other party's phone
// number, so it is reduced to a correlation handle.
func logChatID(jid types.JID) string {
	if jid.Server == types.GroupServer {
		return jid.ToNonAD().String()
	}
	return logx.Digest(jid.ToNonAD().String())
}

// messageFrom classifies an inbound event: kind is one of
// message | reaction | revoke | edit, or "" for something the chat log has no
// place for. For reaction/revoke/edit the target id is returned in
// QuotedMessageID and the new text/emoji in Text.
func messageFrom(e *events.Message) (model.ChatMessage, string) {
	m := model.ChatMessage{
		ID: e.Info.ID, ChatID: e.Info.Chat.ToNonAD().String(), IsFromMe: e.Info.IsFromMe,
		Sender: attendeeFrom(e.Info.Sender, e.Info.SenderAlt, e.Info.PushName),
		SentAt: e.Info.Timestamp.UTC(), Kind: "text", Reactions: []model.Reaction{},
	}
	msg := unwrap(e.Message)
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
		// App-state key shares, history-sync notifications, peer-data responses:
		// machinery, not conversation. An empty kind means "drop it" — storing
		// these would put rows and webhooks in front of the developer for
		// something no WhatsApp client ever shows.
		return m, ""
	case msg.GetConversation() != "":
		m.Text = msg.GetConversation()
	case msg.GetExtendedTextMessage() != nil:
		m.Text = msg.GetExtendedTextMessage().GetText()
		m.QuotedMessageID = msg.GetExtendedTextMessage().GetContextInfo().GetStanzaID()
	default:
		if hasOnlyNoiseFields(msg) {
			// Key-distribution setup is routinely piggybacked on real
			// content in group sends, so it cannot be dropped just because
			// it is present — only when it is ALL that is present. Reaching
			// here already means no recognised content matched, so this is
			// the point to decide "machinery" vs. "content we don't render
			// yet". As with GetProtocolMessage() above, an empty kind means
			// "drop it": no WhatsApp client ever shows this either.
			return m, ""
		}
		m.Kind = "unsupported"
		m.Text = mediaLabel(msg)
	}
	return m, "message"
}

// unwrap peels the envelopes WhatsApp puts around an ordinary body —
// disappearing-message chats (Ephemeral), view-once sends, bot replies and
// the sender's own-device copy — so the callers see the same shape as a
// plain send. Mirrors whatsmeow's UnwrapRaw without needing RawMessage.
func unwrap(msg *waE2E.Message) *waE2E.Message {
	for i := 0; i < 8 && msg != nil; i++ {
		switch {
		case msg.GetDeviceSentMessage().GetMessage() != nil:
			msg = msg.GetDeviceSentMessage().GetMessage()
		case msg.GetBotInvokeMessage().GetMessage() != nil:
			msg = msg.GetBotInvokeMessage().GetMessage()
		case msg.GetEphemeralMessage().GetMessage() != nil:
			msg = msg.GetEphemeralMessage().GetMessage()
		case msg.GetViewOnceMessage().GetMessage() != nil:
			msg = msg.GetViewOnceMessage().GetMessage()
		case msg.GetViewOnceMessageV2().GetMessage() != nil:
			msg = msg.GetViewOnceMessageV2().GetMessage()
		case msg.GetViewOnceMessageV2Extension().GetMessage() != nil:
			msg = msg.GetViewOnceMessageV2Extension().GetMessage()
		case msg.GetDocumentWithCaptionMessage().GetMessage() != nil:
			msg = msg.GetDocumentWithCaptionMessage().GetMessage()
		default:
			return msg
		}
	}
	return msg
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
	withCaption := func(label, caption string) string {
		if caption = strings.TrimSpace(caption); caption != "" {
			return label + " " + caption
		}
		return label
	}
	switch {
	case msg.GetImageMessage() != nil:
		return withCaption("[image]", msg.GetImageMessage().GetCaption())
	case msg.GetVideoMessage() != nil:
		if msg.GetVideoMessage().GetGifPlayback() {
			return withCaption("[gif]", msg.GetVideoMessage().GetCaption())
		}
		return withCaption("[video]", msg.GetVideoMessage().GetCaption())
	case msg.GetPtvMessage() != nil:
		return "[video note]"
	case msg.GetAudioMessage() != nil:
		if msg.GetAudioMessage().GetPTT() {
			return "[voice note]"
		}
		return "[audio]"
	case msg.GetDocumentMessage() != nil:
		d := msg.GetDocumentMessage()
		return withCaption(withCaption("[document]", d.GetFileName()), d.GetCaption())
	case msg.GetStickerMessage() != nil, msg.GetLottieStickerMessage() != nil:
		return "[sticker]"
	case msg.GetAlbumMessage() != nil:
		a := msg.GetAlbumMessage()
		n := a.GetExpectedImageCount() + a.GetExpectedVideoCount()
		switch {
		case n == 0:
			return "[album]"
		case a.GetExpectedVideoCount() == 0:
			return fmt.Sprintf("[album: %d photos]", n)
		default:
			return fmt.Sprintf("[album: %d items]", n)
		}
	case msg.GetLocationMessage() != nil, msg.GetLiveLocationMessage() != nil:
		return "[location]"
	case msg.GetContactMessage() != nil, msg.GetContactsArrayMessage() != nil:
		return "[contact]"
	case msg.GetPollCreationMessage() != nil:
		return withCaption("[poll]", msg.GetPollCreationMessage().GetName())
	case msg.GetPollUpdateMessage() != nil:
		return "[poll vote]"
	case msg.GetEventMessage() != nil:
		return withCaption("[event]", msg.GetEventMessage().GetName())
	case msg.GetGroupInviteMessage() != nil:
		return withCaption("[group invite]", msg.GetGroupInviteMessage().GetGroupName())
	case msg.GetBcallMessage() != nil, msg.GetScheduledCallCreationMessage() != nil:
		return "[call]"
	case msg.GetOrderMessage() != nil, msg.GetProductMessage() != nil:
		return "[catalog item]"
	case msg.GetListMessage() != nil, msg.GetButtonsMessage() != nil, msg.GetTemplateMessage() != nil,
		msg.GetInteractiveMessage() != nil:
		return "[interactive message]"
	case msg.GetListResponseMessage() != nil, msg.GetButtonsResponseMessage() != nil,
		msg.GetTemplateButtonReplyMessage() != nil, msg.GetInteractiveResponseMessage() != nil:
		return "[button reply]"
	}
	// Name the field so an operator can tell from the row or the log what
	// WhatsApp sent, instead of a bare "[unsupported]".
	if name := firstFieldName(msg); name != "" {
		return "[unsupported: " + name + "]"
	}
	return "[unsupported]"
}

// noiseFields are wire fields that carry no user content, ever — protocol
// machinery WhatsApp attaches to a message rather than something any client
// renders. Kept deliberately small: each entry here silently drops a message
// carrying nothing else, so a wrong one loses real user content.
//
//   - senderKeyDistributionMessage: E2E group-session key setup. WhatsApp
//     piggybacks this on real content as often as it sends it bare (see
//     hasOnlyNoiseFields), but on its own it is pure crypto bootstrapping.
//   - messageContextInfo: reply/forward bookkeeping (e.g. stanza ids), never
//     content by itself; already treated as invisible by firstFieldName below.
var noiseFields = map[protoreflect.Name]bool{
	"senderKeyDistributionMessage": true,
	"messageContextInfo":           true,
}

// hasOnlyNoiseFields reports whether every populated field on msg is known
// protocol noise (see noiseFields) — i.e. there is no content on it, matched
// or not, that any chat should ever surface. Callers must check this only
// after every recognised content case has already failed to match, since an
// SKDM commonly rides alongside a real conversation or media message.
func hasOnlyNoiseFields(msg *waE2E.Message) bool {
	if msg == nil {
		return false
	}
	only := true
	msg.ProtoReflect().Range(func(fd protoreflect.FieldDescriptor, _ protoreflect.Value) bool {
		if !noiseFields[fd.Name()] {
			only = false
			return false
		}
		return true
	})
	return only
}

// firstFieldName is the JSON name of the first populated field of a message,
// e.g. "keepInChatMessage" — the wire type we did not translate.
func firstFieldName(msg *waE2E.Message) string {
	if msg == nil {
		return ""
	}
	var name string
	msg.ProtoReflect().Range(func(fd protoreflect.FieldDescriptor, _ protoreflect.Value) bool {
		if fd.Name() == "messageContextInfo" {
			return true
		}
		name = fd.JSONName()
		return false
	})
	return name
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
