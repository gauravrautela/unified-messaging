package providertest

import (
	"context"
	"testing"
	"time"

	"github.com/gauravrautela/unified-messaging/internal/model"
	"github.com/gauravrautela/unified-messaging/internal/provider"
)

type recSink struct {
	msgs []model.ChatMessage
	disc []string
}

func (r *recSink) Message(_ string, m model.ChatMessage, _ model.Chat, _ model.Attendee) {
	r.msgs = append(r.msgs, m)
}
func (r *recSink) Receipt(string, string, []string, string)         {}
func (r *recSink) Reaction(string, string, string, model.Reaction)  {}
func (r *recSink) Edited(string, string, string, string, time.Time) {}
func (r *recSink) Deleted(string, string, string)                   {}
func (r *recSink) Disconnected(_ string, reason string, _ bool)     { r.disc = append(r.disc, reason) }

func TestFakeChatIsAChatProvider(t *testing.T) {
	var p provider.Provider = NewFakeChat("FAKECHAT")
	if p.Kind() != "chat" || p.Linker() == nil || p.Chat() == nil || p.Mailbox() != nil || p.Auth() != nil || p.Push() != nil {
		t.Fatalf("capabilities wrong: kind=%s", p.Kind())
	}
}

func TestFakeChatLinkScript(t *testing.T) {
	f := NewFakeChat("FAKECHAT")
	sess, err := f.Linker().StartLink(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	f.EmitCode("qr-1")
	if c := <-sess.Codes(); c.Code != "qr-1" || c.ExpiresAt.IsZero() {
		t.Fatalf("code = %+v", c)
	}
	f.Pair(provider.Identity{Identifier: "+919888000000", Name: "Test"}, "919888000000:5@s.whatsapp.net")
	res := <-sess.Result()
	if res.Err != nil || res.DeviceJID != "919888000000:5@s.whatsapp.net" || res.Identity.Identifier != "+919888000000" {
		t.Fatalf("result = %+v", res)
	}
}

// TestFakeChatEmitAfterPairIsSafe covers QR rotation racing pairing: a code
// emitted after the session already resolved must be a silent no-op, not a
// panic, and a second Pair on an already-resolved session must not block or
// deliver a second result.
func TestFakeChatEmitAfterPairIsSafe(t *testing.T) {
	f := NewFakeChat("FAKECHAT")
	sess, err := f.Linker().StartLink(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	f.Pair(provider.Identity{Identifier: "+919888000000"}, "919888000000:5@s.whatsapp.net")
	f.EmitCode("qr-late")                                                          // must not panic on the now-closed codes channel
	f.Pair(provider.Identity{Identifier: "+919888000001"}, "other@s.whatsapp.net") // must not block

	select {
	case res := <-sess.Result():
		if res.DeviceJID != "919888000000:5@s.whatsapp.net" {
			t.Fatalf("result = %+v, want the first Pair's result", res)
		}
	default:
		t.Fatal("expected a result to be ready")
	}

	select {
	case res := <-sess.Result():
		t.Fatalf("expected exactly one result, got a second: %+v", res)
	default:
		// Good: the second Pair did not queue a second value.
	}
}

func TestFakeChatConnectRecordsSinkAndCommands(t *testing.T) {
	f := NewFakeChat("FAKECHAT")
	sink := &recSink{}
	conn, err := f.Chat().Connect(context.Background(), "acc_1", "dev@jid", sink)
	if err != nil {
		t.Fatal(err)
	}
	f.Sink("acc_1").Message("acc_1", model.ChatMessage{ID: "M1", Text: "hi"}, model.Chat{ID: "c1"}, model.Attendee{ID: "a1"})
	if len(sink.msgs) != 1 || sink.msgs[0].ID != "M1" {
		t.Fatalf("sink did not receive: %+v", sink.msgs)
	}
	if _, err := f.Chat().SendText(context.Background(), "acc_1", "c1", "hello", ""); err != nil {
		t.Fatal(err)
	}
	if got := f.Commands(); len(got) != 1 || got[0] != "SendText acc_1 c1 hello" {
		t.Fatalf("commands = %v", got)
	}
	f.Disconnect("acc_1", "gone", true)
	if len(sink.disc) != 1 || sink.disc[0] != "gone" {
		t.Fatalf("disconnect not delivered: %v", sink.disc)
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Chat().Forget(context.Background(), "dev@jid"); err != nil || len(f.Forgotten()) != 1 {
		t.Fatalf("forget: %v %v", err, f.Forgotten())
	}
}

func TestChatEventNamesAreKnown(t *testing.T) {
	for _, e := range []string{model.EventChatReceived, model.EventChatSent, model.EventChatUpdated, model.EventChatReaction, model.EventChatDeleted} {
		if !model.KnownEvent(e) {
			t.Errorf("%s not known", e)
		}
	}
}
