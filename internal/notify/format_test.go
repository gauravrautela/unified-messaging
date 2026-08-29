package notify

import (
	"strings"
	"testing"
	"time"

	"github.com/gauravrautela/unified-messaging/internal/model"
)

func TestFormatMailReceivedMarkdownAndHTML(t *testing.T) {
	ev := model.Event{Type: model.EventMailReceived, AccountID: "acc_1", Account: &model.Account{Email: "me@x.com"},
		Email: &model.Email{Subject: "Q3 *plan*", From: model.Recipient{Name: "Bob <b>", Email: "bob@x.com"},
			BodyPlain: strings.Repeat("a", 250)}}
	md := Format(ev, Markdown)
	if !strings.Contains(md, "📧 **New mail** · me@x.com") || !strings.Contains(md, `Q3 \*plan\*`) ||
		!strings.Contains(md, "Bob <b> <bob@x.com>") || !strings.HasSuffix(md, strings.Repeat("a", 200)+"…") {
		t.Fatalf("markdown:\n%s", md)
	}
	h := Format(ev, HTML)
	if !strings.Contains(h, "<b>New mail</b>") || !strings.Contains(h, "Bob &lt;b&gt; &lt;bob@x.com&gt;") || !strings.Contains(h, "<b>Q3 *plan*</b>") {
		t.Fatalf("html:\n%s", h)
	}
}

func TestFormatChatReceivedMasksPhonesAndEscapes(t *testing.T) {
	ev := model.Event{Type: model.EventChatReceived, AccountID: "acc_1",
		Chat:    &model.Chat{Name: "", Kind: "direct"},
		Message: &model.ChatMessage{Sender: model.Attendee{Phone: "+919888000855"}, Text: "hi _there_ <3"}}
	md := Format(ev, Markdown)
	if strings.Contains(md, "9888000855") || !strings.Contains(md, "+91 98••• •855") || !strings.Contains(md, `hi \_there\_ <3`) {
		t.Fatalf("markdown:\n%s", md)
	}
	h := Format(ev, HTML)
	if !strings.Contains(h, "hi _there_ &lt;3") {
		t.Fatalf("html:\n%s", h)
	}
}

func TestFormatOtherEvents(t *testing.T) {
	at := time.Now()
	cases := []struct {
		ev   model.Event
		want string
	}{
		{model.Event{Type: model.EventMailSent, Email: &model.Email{Subject: "S", To: []model.Recipient{{Email: "a@x.com"}}}}, "📤 **Mail sent**"},
		{model.Event{Type: model.EventMailUpdated, Email: &model.Email{Subject: "S", Read: true}}, "✏️ **Mail updated**"},
		{model.Event{Type: model.EventMailDeleted, EmailID: "m1"}, "🗑 **Mail deleted**"},
		{model.Event{Type: model.EventChatUpdated, Change: "edited", Chat: &model.Chat{Name: "Team"}, Message: &model.ChatMessage{Text: "new"}}, "✏️ **Message edited**"},
		{model.Event{Type: model.EventChatUpdated, Change: "receipt", Status: "read", Chat: &model.Chat{Name: "Team"}}, "📬"},
		{model.Event{Type: model.EventChatReaction, Chat: &model.Chat{Name: "Team"}, Reaction: &model.Reaction{Emoji: "👍", At: at}, Message: &model.ChatMessage{Sender: model.Attendee{Name: "Ada"}}}, "👍 **Reaction**"},
		{model.Event{Type: model.EventChatReaction, Chat: &model.Chat{Name: "Team"}, Reaction: &model.Reaction{Emoji: "", At: at}}, "reaction removed"},
		{model.Event{Type: model.EventChatDeleted, Chat: &model.Chat{Name: "Team"}}, "🗑 **Message deleted**"},
		{model.Event{Type: model.EventAccountError, Account: &model.Account{Email: "me@x.com", Status: model.AccountCredentials}}, "⚠️ **Account needs attention**"},
		{model.Event{Type: "something_new", AccountID: "acc_9"}, "something_new · acc_9"},
	}
	for _, c := range cases {
		if got := Format(c.ev, Markdown); !strings.Contains(got, c.want) {
			t.Errorf("%s: got %q, want it to contain %q", c.ev.Type, got, c.want)
		}
	}
}

// The header says where a chat message came from: a group, a Status post,
// a Channel, or a direct chat — the same sender name is not enough.
func TestFormatChatHeaderShowsWhere(t *testing.T) {
	msg := &model.ChatMessage{Sender: model.Attendee{Name: "Vatsal"}, Text: "[image]"}
	cases := []struct {
		chat *model.Chat
		want string
	}{
		{&model.Chat{Kind: "status", Name: "Vatsal"}, "WhatsApp** · Status · Vatsal"},
		{&model.Chat{Kind: "group", Name: "Founders"}, "WhatsApp** · Group: Founders"},
		{&model.Chat{Kind: "channel", ID: "120363179221369609@newsletter"}, "WhatsApp** · Channel · 120363179221369609"},
		{&model.Chat{Kind: "direct", Name: "Vatsal"}, "WhatsApp** · Vatsal"},
	}
	for _, c := range cases {
		md := Format(model.Event{Type: model.EventChatReceived, AccountID: "acc_1", Chat: c.chat, Message: msg}, Markdown)
		if !strings.Contains(md, c.want) {
			t.Errorf("kind %s: want %q in\n%s", c.chat.Kind, c.want, md)
		}
	}
}

// status@broadcast is a single shared pseudo-chat that every contact's
// status posts land in; its stored Chat.Name is a stale, borrowed value from
// whichever contact's post first backfilled it (see internal/notify/format.go
// chatName doc comment). The label must therefore come from the message
// sender, never from the chat's name, no matter what that name says.
func TestFormatStatusUsesSenderNotStaleChatName(t *testing.T) {
	ev := model.Event{
		Type:      model.EventChatReceived,
		AccountID: "acc_1",
		Chat:      &model.Chat{Kind: "status", Name: "Satish Mehra"},
		Message:   &model.ChatMessage{Sender: model.Attendee{Name: "Vishal Gupta"}, Text: "[image]"},
	}
	md := Format(ev, Markdown)
	if !strings.Contains(md, "Status · Vishal Gupta") {
		t.Errorf("want status label to use sender name, got:\n%s", md)
	}
	if strings.Contains(md, "Satish Mehra") {
		t.Errorf("want stale chat name not to leak into label, got:\n%s", md)
	}
}

// Groups have genuine per-chat names; a group event must keep using the
// chat's name, not the sender, so this fix does not regress the group path.
func TestFormatGroupStillUsesChatName(t *testing.T) {
	ev := model.Event{
		Type:      model.EventChatReceived,
		AccountID: "acc_1",
		Chat:      &model.Chat{Kind: "group", Name: "Founders"},
		Message:   &model.ChatMessage{Sender: model.Attendee{Name: "Vatsal"}, Text: "hi"},
	}
	md := Format(ev, Markdown)
	if !strings.Contains(md, "Group: Founders") {
		t.Errorf("want group label to use chat name, got:\n%s", md)
	}
}
