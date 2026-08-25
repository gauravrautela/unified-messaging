package whatsapp

import (
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

func TestAttendeeFromPhoneAndLID(t *testing.T) {
	a := attendeeFrom(types.NewJID("919888000000", types.DefaultUserServer), types.JID{}, "Ada")
	if a.ID != "919888000000@s.whatsapp.net" || a.Phone != "+919888000000" || a.Name != "Ada" {
		t.Fatalf("phone attendee = %+v", a)
	}
	l := attendeeFrom(types.NewJID("234488185487529", types.HiddenUserServer), types.JID{}, "Nim")
	if l.ID != "234488185487529@lid" || l.Phone != "" {
		t.Fatalf("lid attendee = %+v", l)
	}
	// A LID sender whose phone alt is known resolves to the phone id.
	r := attendeeFrom(types.NewJID("234488185487529", types.HiddenUserServer), types.NewJID("919888000001", types.DefaultUserServer), "Nim")
	if r.ID != "919888000001@s.whatsapp.net" || r.Phone != "+919888000001" {
		t.Fatalf("resolved attendee = %+v", r)
	}
}

func TestChatKind(t *testing.T) {
	if chatKind(types.NewJID("1203", types.GroupServer)) != "group" || chatKind(types.NewJID("91", types.DefaultUserServer)) != "direct" {
		t.Fatal("chatKind wrong")
	}
}

func evt(chat types.JID, id string, msg *waE2E.Message) *events.Message {
	return &events.Message{Info: types.MessageInfo{
		MessageSource: types.MessageSource{Chat: chat, Sender: types.NewJID("91", types.DefaultUserServer), IsGroup: chat.Server == types.GroupServer},
		ID:            id, PushName: "Ada", Timestamp: time.Unix(1_700_000_000, 0)}, Message: msg}
}

func TestMessageFromTextQuoteReactionRevokeEditMedia(t *testing.T) {
	chat := types.NewJID("91", types.DefaultUserServer)
	m, kind := messageFrom(evt(chat, "A", &waE2E.Message{Conversation: proto.String("hi")}))
	if kind != "message" || m.Text != "hi" || m.Kind != "text" || m.ChatID != chat.String() || m.Sender.Name != "Ada" {
		t.Fatalf("text = %+v kind=%s", m, kind)
	}
	m, _ = messageFrom(evt(chat, "B", &waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{
		Text: proto.String("reply"), ContextInfo: &waE2E.ContextInfo{StanzaID: proto.String("A")}}}))
	if m.Text != "reply" || m.QuotedMessageID != "A" {
		t.Fatalf("quote = %+v", m)
	}
	m, kind = messageFrom(evt(chat, "C", &waE2E.Message{ReactionMessage: &waE2E.ReactionMessage{
		Key: &waCommon.MessageKey{ID: proto.String("A")}, Text: proto.String("👍")}}))
	if kind != "reaction" || m.QuotedMessageID != "A" || m.Text != "👍" {
		t.Fatalf("reaction = %+v kind=%s", m, kind)
	}
	m, kind = messageFrom(evt(chat, "D", &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{
		Type: waE2E.ProtocolMessage_REVOKE.Enum(), Key: &waCommon.MessageKey{ID: proto.String("A")}}}))
	if kind != "revoke" || m.QuotedMessageID != "A" {
		t.Fatalf("revoke = %+v kind=%s", m, kind)
	}
	m, kind = messageFrom(evt(chat, "E", &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{
		Type: waE2E.ProtocolMessage_MESSAGE_EDIT.Enum(), Key: &waCommon.MessageKey{ID: proto.String("A")},
		EditedMessage: &waE2E.Message{Conversation: proto.String("hi!")}}}))
	if kind != "edit" || m.QuotedMessageID != "A" || m.Text != "hi!" {
		t.Fatalf("edit = %+v kind=%s", m, kind)
	}
	m, kind = messageFrom(evt(chat, "F", &waE2E.Message{ImageMessage: &waE2E.ImageMessage{}}))
	if kind != "message" || m.Kind != "unsupported" || m.Text != "[image]" {
		t.Fatalf("media = %+v", m)
	}
	// Protocol traffic that is not a revoke or an edit is machinery: an empty
	// kind means the connection drops it rather than storing a chat row.
	_, kind = messageFrom(evt(chat, "G", &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{
		Type: waE2E.ProtocolMessage_APP_STATE_SYNC_KEY_SHARE.Enum(), Key: &waCommon.MessageKey{ID: proto.String("A")}}}))
	if kind != "" {
		t.Fatalf("app-state key share kind = %q, want \"\"", kind)
	}
}

// Group chat ids are opaque and log verbatim; a direct chat id is the other
// party's phone number and must only appear as a digest.
func TestLogChatID(t *testing.T) {
	group := types.NewJID("120363000000000000", types.GroupServer)
	if got := logChatID(group); got != group.String() {
		t.Fatalf("group log id = %q, want %q", got, group.String())
	}
	direct := types.NewJID("919888000000", types.DefaultUserServer)
	got := logChatID(direct)
	if strings.Contains(got, "919888000000") || !strings.HasPrefix(got, "h_") {
		t.Fatalf("direct log id = %q, want a digest", got)
	}
	if got != logChatID(types.NewJID("919888000000", types.DefaultUserServer)) {
		t.Fatal("digest must be stable so lines can be correlated")
	}
}

func TestReceiptStatus(t *testing.T) {
	if s, ok := receiptStatus(types.ReceiptTypeDelivered); !ok || s != "delivered" {
		t.Fatal("delivered")
	}
	if s, ok := receiptStatus(types.ReceiptTypeRead); !ok || s != "read" {
		t.Fatal("read")
	}
	if _, ok := receiptStatus(types.ReceiptTypeSender); ok {
		t.Fatal("sender receipts must be ignored")
	}
}
