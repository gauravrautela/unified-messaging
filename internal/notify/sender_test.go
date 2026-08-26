package notify

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gauravrautela/unified-messaging/internal/model"
)

func capture(t *testing.T, code int, body string) (*httptest.Server, *[]map[string]any, *http.Header) {
	t.Helper()
	var got []map[string]any
	var hdr http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(raw, &m)
		m["_path"] = r.URL.Path
		got = append(got, m)
		hdr = r.Header.Clone()
		w.WriteHeader(code)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, &got, &hdr
}

var chatEv = model.Event{Type: model.EventChatReceived, AccountID: "acc_1",
	Chat: &model.Chat{Name: "Team"}, Message: &model.ChatMessage{Sender: model.Attendee{Name: "Ada"}, Text: "hello *world*"}}

func TestWebhookSenderKeepsHeadersAndSignature(t *testing.T) {
	srv, got, hdr := capture(t, 200, "")
	s, _ := NewRegistry(srv.Client()).For(model.WebhookKindWebhook)
	h := model.Webhook{Kind: model.WebhookKindWebhook, URL: srv.URL, Secret: "s3"}
	if err := s.Send(context.Background(), h, chatEv, []byte(`{"type":"chat_received"}`), 2); err != nil {
		t.Fatal(err)
	}
	if (*got)[0]["type"] != "chat_received" || hdr.Get("X-Outlook-Event") != "chat_received" ||
		hdr.Get("X-Outlook-Delivery") != "2" || !strings.HasPrefix(hdr.Get("X-Outlook-Signature"), "sha256=") {
		t.Fatalf("got %v headers %v", *got, *hdr)
	}
}

func TestDiscordSenderPostsFormattedContentWithoutMentions(t *testing.T) {
	srv, got, _ := capture(t, 204, "")
	reg := NewRegistry(srv.Client())
	s, _ := reg.For(model.WebhookKindDiscord)
	err := s.Send(context.Background(), model.Webhook{Kind: model.WebhookKindDiscord, URL: srv.URL + "/api/webhooks/1/tok"}, chatEv, nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	m := (*got)[0]
	content, _ := m["content"].(string)
	if !strings.Contains(content, `hello \*world\*`) || !strings.Contains(content, "**Ada**") {
		t.Fatalf("content = %q", content)
	}
	am, _ := m["allowed_mentions"].(map[string]any)
	if parse, ok := am["parse"].([]any); !ok || len(parse) != 0 {
		t.Fatalf("allowed_mentions = %v", m["allowed_mentions"])
	}
}

func TestDiscordSenderTreats429AsFailureAndScrubsURL(t *testing.T) {
	srv, _, _ := capture(t, 429, `{"retry_after":1.2}`)
	s, _ := NewRegistry(srv.Client()).For(model.WebhookKindDiscord)
	err := s.Send(context.Background(), model.Webhook{Kind: model.WebhookKindDiscord, URL: srv.URL + "/api/webhooks/1/s3cr3t"}, chatEv, nil, 1)
	if err == nil || !strings.Contains(err.Error(), "429") || strings.Contains(err.Error(), "s3cr3t") {
		t.Fatalf("err = %v", err)
	}
}

func TestTelegramSenderUsesBotTokenPathAndHTML(t *testing.T) {
	srv, got, _ := capture(t, 200, `{"ok":true}`)
	reg := NewRegistry(srv.Client())
	reg.SetTelegramBase(srv.URL)
	s, _ := reg.For(model.WebhookKindTelegram)
	h := model.Webhook{Kind: model.WebhookKindTelegram, Telegram: &model.TelegramTarget{BotToken: "123:ABC", ChatID: "-100"}}
	if err := s.Send(context.Background(), h, chatEv, nil, 1); err != nil {
		t.Fatal(err)
	}
	m := (*got)[0]
	if m["_path"] != "/bot123:ABC/sendMessage" || m["chat_id"] != "-100" || m["parse_mode"] != "HTML" ||
		m["disable_web_page_preview"] != true || !strings.Contains(m["text"].(string), "<b>Ada</b>") {
		t.Fatalf("request = %v", m)
	}
}

func TestTelegramSenderErrorsCarryDescriptionNotToken(t *testing.T) {
	srv, _, _ := capture(t, 400, `{"ok":false,"description":"Bad Request: chat not found"}`)
	reg := NewRegistry(srv.Client())
	reg.SetTelegramBase(srv.URL)
	s, _ := reg.For(model.WebhookKindTelegram)
	h := model.Webhook{Kind: model.WebhookKindTelegram, Telegram: &model.TelegramTarget{BotToken: "123:ABC", ChatID: "-100"}}
	err := s.Send(context.Background(), h, chatEv, nil, 1)
	if err == nil || !strings.Contains(err.Error(), "chat not found") || strings.Contains(err.Error(), "123:ABC") {
		t.Fatalf("err = %v", err)
	}
	// A hook whose config could not be unsealed fails clearly, not with a panic.
	err = s.Send(context.Background(), model.Webhook{Kind: model.WebhookKindTelegram, Telegram: &model.TelegramTarget{}}, chatEv, nil, 1)
	if err == nil || !strings.Contains(err.Error(), "config unreadable") {
		t.Fatalf("empty config err = %v", err)
	}
}

func TestValidateTelegram(t *testing.T) {
	ok, _, _ := capture(t, 200, `{"ok":true,"result":{"id":-100}}`)
	reg := NewRegistry(ok.Client())
	reg.SetTelegramBase(ok.URL)
	if err := reg.ValidateTelegram(context.Background(), "1:A", "-100"); err != nil {
		t.Fatal(err)
	}
	bad, _, _ := capture(t, 400, `{"ok":false,"description":"chat not found"}`)
	reg = NewRegistry(bad.Client())
	reg.SetTelegramBase(bad.URL)
	err := reg.ValidateTelegram(context.Background(), "1:A", "-100")
	if !errors.Is(err, ErrTelegramRejected) || !strings.Contains(err.Error(), "chat not found") {
		t.Fatalf("err = %v", err)
	}
	reg = NewRegistry(&http.Client{})
	reg.SetTelegramBase("http://127.0.0.1:1")
	if err := reg.ValidateTelegram(context.Background(), "1:A", "-100"); err == nil || errors.Is(err, ErrTelegramRejected) {
		t.Fatalf("unreachable must be a transport error, got %v", err)
	}
}

func TestUnknownKind(t *testing.T) {
	if _, ok := NewRegistry(&http.Client{}).For("slack"); ok {
		t.Fatal("slack is not a kind")
	}
}
