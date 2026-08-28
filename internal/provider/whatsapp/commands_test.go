package whatsapp

import (
	"testing"

	"go.mau.fi/whatsmeow/types"
)

// This file tests only the pure builders in commands.go: no client, no
// socket, no store. The commands themselves (SendText, React, ...) need a
// live *whatsmeow.Client and are exercised by hand against a real device, the
// same as Connect and StartLink.

func TestTextMessageBuilderQuotes(t *testing.T) {
	m := textMessage("hi", "", types.JID{})
	if m.GetConversation() != "hi" || m.GetExtendedTextMessage() != nil {
		t.Fatalf("plain = %+v", m)
	}
	q := textMessage("re", "A", types.NewJID("91", types.DefaultUserServer))
	if q.GetExtendedTextMessage().GetText() != "re" || q.GetExtendedTextMessage().GetContextInfo().GetStanzaID() != "A" {
		t.Fatalf("quoted = %+v", q)
	}
}

func TestMessageIDsConvert(t *testing.T) {
	ids := toMessageIDs([]string{"A", "B"})
	if len(ids) != 2 || ids[0] != types.MessageID("A") || ids[1] != types.MessageID("B") {
		t.Fatalf("toMessageIDs = %+v", ids)
	}
	if got := toMessageIDs(nil); len(got) != 0 {
		t.Fatalf("toMessageIDs(nil) = %+v, want empty", got)
	}
}
