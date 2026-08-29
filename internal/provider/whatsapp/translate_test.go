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

	"github.com/gauravrautela/unified-messaging/internal/model"
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

// senderKeyDistributionMessage is E2E session-bootstrapping machinery, never
// something a WhatsApp client shows. A message that carries nothing else must
// be dropped the same way an app-state key share is: an empty kind.
func TestMessageFromDropsBareSenderKeyDistribution(t *testing.T) {
	chat := types.NewJID("120363000000000000", types.GroupServer)
	_, kind := messageFrom(evt(chat, "J", &waE2E.Message{
		SenderKeyDistributionMessage: &waE2E.SenderKeyDistributionMessage{GroupID: proto.String("g1")},
	}))
	if kind != "" {
		t.Fatalf("bare SKDM kind = %q, want \"\"", kind)
	}
}

// WhatsApp routinely piggybacks a senderKeyDistributionMessage on an ordinary
// group send. Dropping whenever an SKDM is merely present — rather than
// whenever it is the ONLY thing present — would silently discard real
// messages, which is worse than the bug being fixed. This is the regression
// guard for that trap.
func TestMessageFromKeepsConversationAlongsideSenderKeyDistribution(t *testing.T) {
	chat := types.NewJID("120363000000000000", types.GroupServer)
	m, kind := messageFrom(evt(chat, "K", &waE2E.Message{
		SenderKeyDistributionMessage: &waE2E.SenderKeyDistributionMessage{GroupID: proto.String("g1")},
		Conversation:                 proto.String("hi team"),
	}))
	if kind != "message" || m.Text != "hi team" || m.Kind != "text" {
		t.Fatalf("SKDM + conversation = %+v kind=%s, want text kept", m, kind)
	}
}

// An SKDM riding alongside real media content (not just text) must likewise
// still be stored — the "drop" path is for pure machinery only.
func TestMessageFromKeepsMediaAlongsideSenderKeyDistribution(t *testing.T) {
	chat := types.NewJID("120363000000000000", types.GroupServer)
	m, kind := messageFrom(evt(chat, "L", &waE2E.Message{
		SenderKeyDistributionMessage: &waE2E.SenderKeyDistributionMessage{GroupID: proto.String("g1")},
		VideoMessage:                 &waE2E.VideoMessage{},
	}))
	if kind != "message" || m.Kind != "unsupported" || m.Text != "[video]" {
		t.Fatalf("SKDM + video = %+v kind=%s, want unsupported video kept", m, kind)
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

// The roster is built from two passes — group participants first (which
// resolves names through the contact store) and then the contact list. A
// contact with no name at all must not blank the name the group pass already
// resolved for the same person.
func TestRememberAttendeeKeepsAResolvedName(t *testing.T) {
	seen := map[string]model.Attendee{}
	resolved := model.Attendee{ID: "919888000000@s.whatsapp.net", Phone: "+919888000000", Name: "Ada"}
	rememberAttendee(seen, resolved)
	rememberAttendee(seen, model.Attendee{ID: "919888000000@s.whatsapp.net"})
	if got := seen[resolved.ID]; got.Name != "Ada" || got.Phone != "+919888000000" {
		t.Fatalf("a barer record blanked a resolved name or phone: %+v", got)
	}
	// A name arriving later still wins over a blank one.
	seen = map[string]model.Attendee{}
	rememberAttendee(seen, model.Attendee{ID: resolved.ID})
	rememberAttendee(seen, resolved)
	if got := seen[resolved.ID]; got.Name != "Ada" {
		t.Fatalf("resolved name did not replace the blank one: %+v", got)
	}
	// Anything else about the newer record still overwrites: only the name is
	// protected, and only against being blanked.
	rememberAttendee(seen, model.Attendee{ID: resolved.ID, Name: "Ada L", IsSelf: true})
	if got := seen[resolved.ID]; got.Name != "Ada L" || !got.IsSelf {
		t.Fatalf("newer record did not win: %+v", got)
	}
}

// Disappearing-message groups and view-once sends wrap the real body in an
// envelope; the text must come out the same as an unwrapped send.
func TestMessageFromUnwrapsEphemeralAndViewOnce(t *testing.T) {
	chat := types.NewJID("120363000000000000", types.GroupServer)
	m, kind := messageFrom(evt(chat, "H", &waE2E.Message{EphemeralMessage: &waE2E.FutureProofMessage{
		Message: &waE2E.Message{Conversation: proto.String("vanishing")}}}))
	if kind != "message" || m.Kind != "text" || m.Text != "vanishing" {
		t.Fatalf("ephemeral: kind=%q m.Kind=%q text=%q", kind, m.Kind, m.Text)
	}
	m, _ = messageFrom(evt(chat, "I", &waE2E.Message{ViewOnceMessageV2: &waE2E.FutureProofMessage{
		Message: &waE2E.Message{ImageMessage: &waE2E.ImageMessage{}}}}))
	if m.Kind != "unsupported" || m.Text != "[image]" {
		t.Fatalf("view-once image: kind=%q text=%q", m.Kind, m.Text)
	}
}

func TestChatKindStatusAndChannel(t *testing.T) {
	if got := chatKind(types.StatusBroadcastJID); got != "status" {
		t.Fatalf("status = %q", got)
	}
	if got := chatKind(types.NewJID("120363179221369609", types.NewsletterServer)); got != "channel" {
		t.Fatalf("channel = %q", got)
	}
}

// Media that is not text still tells the reader what it was, and carries the
// caption when there is one. The generic fallback names the proto field so
// a new kind of message is diagnosable from a log line.
func TestMediaLabelsAndCaptions(t *testing.T) {
	cases := []struct {
		msg  *waE2E.Message
		want string
	}{
		{&waE2E.Message{ImageMessage: &waE2E.ImageMessage{Caption: proto.String("look")}}, "[image] look"},
		{&waE2E.Message{VideoMessage: &waE2E.VideoMessage{GifPlayback: proto.Bool(true)}}, "[gif]"},
		{&waE2E.Message{PtvMessage: &waE2E.VideoMessage{}}, "[video note]"},
		{&waE2E.Message{AlbumMessage: &waE2E.AlbumMessage{ExpectedImageCount: proto.Uint32(4)}}, "[album: 4 photos]"},
		{&waE2E.Message{EventMessage: &waE2E.EventMessage{Name: proto.String("Offsite")}}, "[event] Offsite"},
		{&waE2E.Message{PollUpdateMessage: &waE2E.PollUpdateMessage{}}, "[poll vote]"},
		{&waE2E.Message{DocumentMessage: &waE2E.DocumentMessage{FileName: proto.String("q3.pdf")}}, "[document] q3.pdf"},
		{&waE2E.Message{GroupInviteMessage: &waE2E.GroupInviteMessage{GroupName: proto.String("Founders")}}, "[group invite] Founders"},
		{&waE2E.Message{StickerMessage: &waE2E.StickerMessage{}}, "[sticker]"},
		{&waE2E.Message{KeepInChatMessage: &waE2E.KeepInChatMessage{}}, "[unsupported: keepInChatMessage]"},
	}
	for _, c := range cases {
		if got := mediaLabel(c.msg); got != c.want {
			t.Errorf("mediaLabel = %q, want %q", got, c.want)
		}
	}
}
