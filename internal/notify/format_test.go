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
