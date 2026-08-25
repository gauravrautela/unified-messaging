package whatsapp

import (
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
