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
	s := store.OpenForTest(t)
	s.SetSealKey(testKey)
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
	s := store.OpenForTest(t)
	_ = s.CreateDeveloper(model.Developer{ID: "dev_1", Email: "d@x.com"}, "h")
	err := s.SaveWebhook(model.Webhook{ID: "wh_1", DeveloperID: "dev_1", Kind: model.WebhookKindTelegram,
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
	// Pinned to sqlite: this test needs a second store over the same data,
	// which OpenForTest cannot give (each call gets its own database).
	dbPath := filepath.Join(t.TempDir(), "w.db")

	s1, err := store.Open("sqlite", dbPath)
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
	s2, err := store.Open("sqlite", dbPath)
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

	// ListWebhooksFor runs on every dispatched event, so the WARN has to be
	// once per webhook id, not once per query: otherwise one un-openable row
	// is a log line per event, forever. Later queries drop to DEBUG.
	for i := 0; i < 3; i++ {
		if _, err := s2.ListWebhooks("dev_1"); err != nil {
			t.Fatal(err)
		}
	}
	warns := 0
	for _, l := range recs.All() {
		if contains(l, "webhook config unreadable") && contains(l, "level=WARN") {
			warns++
		}
	}
	if warns != 1 {
		t.Fatalf("webhook config unreadable warned %d times, want exactly 1: %v", warns, recs.All())
	}
}

// A pending connect-time hook that cannot be unsealed — TOKEN_ENCRYPTION_KEY
// rotated, or a second instance with a different key — must not vanish in
// silence: the account gets created and the hook the developer asked for is
// never bound, and that has to be visible in the log (without the token).
func TestPendingWebhookUnreadableWarns(t *testing.T) {
	// Pinned to sqlite for the same reason as the test above.
	dbPath := filepath.Join(t.TempDir(), "w.db")
	s1, err := store.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	s1.SetSealKey(testKey)
	if err := s1.CreateDeveloper(model.Developer{ID: "dev_1", Email: "d@x.com"}, "h"); err != nil {
		t.Fatal(err)
	}
	if err := s1.SaveOAuthState(store.OAuthState{State: "st_1", DeveloperID: "dev_1", Provider: "OUTLOOK",
		Webhook:   &store.PendingWebhook{Kind: "telegram", BotToken: "123:ABC", ChatID: "-100"},
		ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if err := s1.Close(); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name   string
		key    []byte
		reason string
	}{
		{"no key at all", nil, "no seal key"},
		{"the wrong key", []byte("ffffffffffffffff0123456789abcdef"), "open failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s2, err := store.Open("sqlite", dbPath)
			if err != nil {
				t.Fatal(err)
			}
			defer s2.Close()
			if tc.key != nil {
				s2.SetSealKey(tc.key)
			}
			log, recs := logx.Capture()
			s2.SetLogger(log)

			o, err := s2.PeekOAuthState("st_1")
			if err != nil {
				t.Fatal(err)
			}
			if o.Webhook != nil {
				t.Fatalf("expected no pending webhook, got %+v", o.Webhook)
			}
			if !recs.Contains("pending webhook unreadable") || !recs.Contains(tc.reason) {
				t.Fatalf("expected a warning with reason %q, got: %v", tc.reason, recs.All())
			}
			for _, l := range recs.All() {
				if contains(l, "123:ABC") {
					t.Fatalf("log leaked the bot token: %q", l)
				}
			}
		})
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

// Pausing and resuming change only the flag: the hook's setup and its
// delivery log come through both untouched, and another developer's hook is
// not found.
func TestSetWebhookPausedKeepsSetupAndDeliveries(t *testing.T) {
	s := openWithKey(t)
	in := model.Webhook{ID: "wh_1", DeveloperID: "dev_1", Name: "ops", URL: "https://h.example.com",
		Secret: "s3cret", Events: []string{"mail_received"}, CreatedAt: time.Now().UTC().Truncate(time.Second)}
	if err := s.SaveWebhook(in); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := s.SaveDelivery(store.Delivery{ID: "dl_1", WebhookID: "wh_1", EventType: "mail_received",
		Payload: []byte(`{}`), Attempts: 8, Dead: true, NextAttemptAt: now, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}

	for _, paused := range []bool{true, true, false, false} {
		if err := s.SetWebhookPaused("dev_1", "wh_1", paused); err != nil {
			t.Fatalf("paused=%v: %v", paused, err)
		}
		got, err := s.GetWebhook("dev_1", "wh_1")
		if err != nil {
			t.Fatal(err)
		}
		if got.Paused != paused {
			t.Fatalf("paused = %v, want %v", got.Paused, paused)
		}
		got.Paused = false
		if got.Name != in.Name || got.URL != in.URL || got.Secret != in.Secret ||
			len(got.Events) != 1 || got.Events[0] != "mail_received" || !got.CreatedAt.Equal(in.CreatedAt) {
			t.Fatalf("setup changed: %+v", got)
		}
		if q, _ := s.ListDeliveries("wh_1", 10, 0); len(q) != 1 || q[0].ID != "dl_1" {
			t.Fatalf("delivery log changed: %+v", q)
		}
	}

	if err := s.CreateDeveloper(model.Developer{ID: "dev_2", Email: "e@x.com"}, "h"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetWebhookPaused("dev_2", "wh_1", true); err != store.ErrNotFound {
		t.Fatalf("other developer: err = %v, want ErrNotFound", err)
	}
	if err := s.SetWebhookPaused("dev_1", "wh_nope", true); err != store.ErrNotFound {
		t.Fatalf("unknown hook: err = %v, want ErrNotFound", err)
	}
}

// A paused hook's due retries are not handed to the retry loop, and are again
// once it is resumed, attempts unchanged.
func TestDueDeliveriesSkipsPausedHooks(t *testing.T) {
	s := openWithKey(t)
	now := time.Now().UTC()
	for _, id := range []string{"wh_1", "wh_2"} {
		if err := s.SaveWebhook(model.Webhook{ID: id, DeveloperID: "dev_1", URL: "https://h.example.com", CreatedAt: now}); err != nil {
			t.Fatal(err)
		}
		if err := s.SaveDelivery(store.Delivery{ID: "dl_" + id, WebhookID: id, EventType: "mail_received",
			Payload: []byte(`{}`), Attempts: 2, NextAttemptAt: now.Add(-time.Minute), CreatedAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.SetWebhookPaused("dev_1", "wh_1", true); err != nil {
		t.Fatal(err)
	}
	due, err := s.DueDeliveries(now, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 1 || due[0].WebhookID != "wh_2" {
		t.Fatalf("due while wh_1 paused = %+v", due)
	}
	if err := s.SetWebhookPaused("dev_1", "wh_1", false); err != nil {
		t.Fatal(err)
	}
	due, _ = s.DueDeliveries(now, 10)
	if len(due) != 2 {
		t.Fatalf("due after resume = %+v", due)
	}
	for _, dl := range due {
		if dl.Attempts != 2 {
			t.Fatalf("attempts changed while paused: %+v", dl)
		}
	}
}
