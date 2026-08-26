package store_test

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/gauravrautela/unified-messaging/internal/logx"
	"github.com/gauravrautela/unified-messaging/internal/model"
	"github.com/gauravrautela/unified-messaging/internal/store"
)

var testKey = []byte("0123456789abcdef0123456789abcdef")

func openWithKey(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "w.db"))
	if err != nil {
		t.Fatal(err)
	}
	s.SetSealKey(testKey)
	t.Cleanup(func() { s.Close() })
	if err := s.CreateDeveloper(model.Developer{ID: "dev_1", Email: "d@x.com"}, "h"); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestWebhookTelegramConfigRoundTripsSealed(t *testing.T) {
	s := openWithKey(t)
	in := model.Webhook{ID: "wh_1", DeveloperID: "dev_1", Kind: model.WebhookKindTelegram,
		Telegram: &model.TelegramTarget{ChatID: "-100123", BotToken: "123:ABC"},
		Events:   []string{"chat_received"}, CreatedAt: time.Now().UTC()}
	if err := s.SaveWebhook(in); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetWebhook("dev_1", "wh_1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Kind != "telegram" || got.Telegram == nil || got.Telegram.ChatID != "-100123" || got.Telegram.BotToken != "123:ABC" {
		t.Fatalf("round trip = %+v (%+v)", got, got.Telegram)
	}
	// The raw column must not contain the token in clear.
	var raw string
	if err := s.DB().QueryRow(`SELECT config FROM webhooks WHERE id = 'wh_1'`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if raw == "" || contains(raw, "123:ABC") {
		t.Fatalf("config stored unsealed: %q", raw)
	}
}

func TestWebhookDefaultsToKindWebhookForLegacyRows(t *testing.T) {
	s := openWithKey(t)
	// Simulate a row written before the columns existed: only the old columns.
	if _, err := s.DB().Exec(`INSERT INTO webhooks (id, developer_id, account_id, name, url, secret, events_json, created_at)
		VALUES ('wh_old','dev_1','','','https://h.example.com','','[]',1)`); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetWebhook("dev_1", "wh_old")
	if err != nil {
		t.Fatal(err)
	}
	if got.Kind != model.WebhookKindWebhook || got.Telegram != nil {
		t.Fatalf("legacy row = %+v", got)
	}
}

func TestSaveTelegramWebhookWithoutSealKeyFails(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "w.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	_ = s.CreateDeveloper(model.Developer{ID: "dev_1", Email: "d@x.com"}, "h")
	err = s.SaveWebhook(model.Webhook{ID: "wh_1", DeveloperID: "dev_1", Kind: model.WebhookKindTelegram,
		Telegram: &model.TelegramTarget{ChatID: "1", BotToken: "t"}, CreatedAt: time.Now()})
	if err == nil {
		t.Fatal("expected an error without a seal key")
	}
}

// A telegram hook makes no sense without somewhere to deliver to: the store
// is the enforcement point, not the caller.
func TestSaveTelegramWebhookWithoutTargetFails(t *testing.T) {
	s := openWithKey(t)
	err := s.SaveWebhook(model.Webhook{ID: "wh_1", DeveloperID: "dev_1", Kind: model.WebhookKindTelegram,
		CreatedAt: time.Now()})
	if err == nil {
		t.Fatal("expected an error for a telegram hook without a target")
	}
}

// A hook saved under one seal key must stay listable — with an empty
// Telegram target, never a stale token — from a process that opens the same
// database without that key, and must log exactly one warning that never
// carries the token.
func TestWebhookConfigUnreadableWarnsWithoutSealKey(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "w.db")

	s1, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	s1.SetSealKey(testKey)
	if err := s1.CreateDeveloper(model.Developer{ID: "dev_1", Email: "d@x.com"}, "h"); err != nil {
		t.Fatal(err)
	}
	if err := s1.SaveWebhook(model.Webhook{ID: "wh_1", DeveloperID: "dev_1", Kind: model.WebhookKindTelegram,
		Telegram: &model.TelegramTarget{ChatID: "-100123", BotToken: "123:ABC"}, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := s1.Close(); err != nil {
		t.Fatal(err)
	}

	// A second store on the same file, deliberately never given the key.
	s2, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	log, recs := logx.Capture()
	s2.SetLogger(log)

	hooks, err := s2.ListWebhooks("dev_1")
	if err != nil {
		t.Fatal(err)
	}
	if len(hooks) != 1 || hooks[0].Telegram == nil ||
		hooks[0].Telegram.BotToken != "" || hooks[0].Telegram.ChatID != "" {
		t.Fatalf("expected a listable hook with an empty Telegram target, got %+v", hooks)
	}
	if !recs.Contains("webhook config unreadable") {
		t.Fatalf("expected a warning, got: %v", recs.All())
	}
	for _, l := range recs.All() {
		if contains(l, "123:ABC") {
			t.Fatalf("log leaked the bot token: %q", l)
		}
	}
}

func contains(s, sub string) bool { return len(sub) > 0 && len(s) >= len(sub) && indexOf(s, sub) >= 0 }
func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
