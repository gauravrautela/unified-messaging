package store

import (
	"testing"
	"time"

	"github.com/gauravrautela/unified-messaging/internal/model"
)

// storeEmailAgedDays writes a message and backdates its stored_at, which is the
// only clock the sweep looks at. The message's own Date is deliberately recent,
// so a sweep that keyed on the provider timestamp instead would fail this test.
func storeEmailAgedDays(t *testing.T, s *Store, acct, id string, days int) {
	t.Helper()
	if err := s.UpsertEmail(model.Email{
		AccountID: acct, ID: id, Subject: "subject " + id, Body: "body " + id,
		From: model.Recipient{Email: "alice@example.com"}, Date: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	aged := time.Now().Add(-time.Duration(days) * 24 * time.Hour).Unix()
	if _, err := s.DB().Exec(s.Q(`UPDATE emails SET stored_at = ? WHERE account_id = ? AND id = ?`), aged, acct, id); err != nil {
		t.Fatal(err)
	}
}

func TestEvictExpiredContentRespectsThePolicyWindow(t *testing.T) {
	s := newTestStore(t)
	acct := seedAccount(t, s)
	storeEmailAgedDays(t, s, acct, "old", 10)
	storeEmailAgedDays(t, s, acct, "new", 1)

	if err := s.SetRetentionMaxAge("dev_1", int64((7 * 24 * time.Hour).Seconds())); err != nil {
		t.Fatal(err)
	}
	n, err := s.EvictExpiredContent(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("EvictExpiredContent evicted %d rows, want 1 (only the 10-day-old one)", n)
	}

	old, err := s.GetEmail(acct, "old")
	if err != nil {
		t.Fatal(err)
	}
	if !old.ContentEvicted || old.Body != "" {
		t.Errorf("old message not evicted: evicted=%v body=%q", old.ContentEvicted, old.Body)
	}
	fresh, err := s.GetEmail(acct, "new")
	if err != nil {
		t.Fatal(err)
	}
	if fresh.ContentEvicted || fresh.Body == "" {
		t.Errorf("message inside the window was evicted: evicted=%v body=%q", fresh.ContentEvicted, fresh.Body)
	}
}

func TestEvictExpiredContentIsANoOpWhenRetentionIsOff(t *testing.T) {
	s := newTestStore(t)
	acct := seedAccount(t, s)
	storeEmailAgedDays(t, s, acct, "ancient", 4000)

	n, err := s.EvictExpiredContent(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("evicted %d rows with retention off, want 0", n)
	}
	got, err := s.GetEmail(acct, "ancient")
	if err != nil {
		t.Fatal(err)
	}
	if got.Body == "" {
		t.Error("content evicted although the developer has no retention policy")
	}
}

// The sweep is the only trigger a developer with no webhooks ever gets, and it
// must stay inside its own tenant.
func TestEvictExpiredContentIsScopedToTheDeveloper(t *testing.T) {
	s := newTestStore(t)
	acctA := seedAccount(t, s) // dev_1 / acc_1
	seedDeveloper(t, s, "dev_2", "dev2@example.com")
	if err := s.UpsertAccount(model.Account{
		ID: "acc_2", DeveloperID: "dev_2", Provider: "OUTLOOK",
		Email: "other@outlook.com", Status: model.AccountOK,
	}); err != nil {
		t.Fatal(err)
	}
	storeEmailAgedDays(t, s, acctA, "a", 10)
	storeEmailAgedDays(t, s, "acc_2", "b", 10)

	if err := s.SetRetentionMaxAge("dev_1", 3600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.EvictExpiredContent(time.Now()); err != nil {
		t.Fatal(err)
	}

	other, err := s.GetEmail("acc_2", "b")
	if err != nil {
		t.Fatal(err)
	}
	if other.ContentEvicted {
		t.Error("evicted another developer's message")
	}
}

func TestEvictExpiredContentSweepsChatMessages(t *testing.T) {
	s := newTestStore(t)
	acct := seedChatAccount(t, s)
	if err := s.UpsertChat(model.Chat{AccountID: acct, ID: "c1", Kind: "dm"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpsertChatMessage(model.ChatMessage{
		AccountID: acct, ID: "cm1", ChatID: "c1", Kind: "text", Text: "old news", SentAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	aged := time.Now().Add(-10 * 24 * time.Hour).Unix()
	if _, err := s.DB().Exec(s.Q(`UPDATE chat_messages SET stored_at = ? WHERE account_id = ? AND id = ?`), aged, acct, "cm1"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetRetentionMaxAge("dev_1", int64((24 * time.Hour).Seconds())); err != nil {
		t.Fatal(err)
	}
	if _, err := s.EvictExpiredContent(time.Now()); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetChatMessage(acct, "cm1")
	if err != nil {
		t.Fatal(err)
	}
	if !got.ContentEvicted || got.Text != "" {
		t.Fatalf("chat message not evicted: evicted=%v text=%q", got.ContentEvicted, got.Text)
	}
}

func seedDeadDelivery(t *testing.T, s *Store, id, webhookID, accountID string, ageDays int) {
	t.Helper()
	if err := s.SaveDelivery(Delivery{
		ID: id, WebhookID: webhookID, AccountID: accountID, EventType: "mail_received",
		Payload: []byte(`{"type":"mail_received"}`), Attempts: 8, Dead: true,
		CreatedAt: time.Now().Add(-time.Duration(ageDays) * 24 * time.Hour).UTC(),
	}); err != nil {
		t.Fatal(err)
	}
}

func TestPurgeDeadDeliveriesHonoursAShorterTenantPolicy(t *testing.T) {
	s := newTestStore(t)
	acct := seedAccount(t, s)
	hook := model.Webhook{ID: "wh_1", DeveloperID: "dev_1", AccountID: acct, URL: "https://example.com/hook", Events: []string{"*"}}
	if err := s.SaveWebhook(hook); err != nil {
		t.Fatal(err)
	}
	seedDeadDelivery(t, s, "dl_1", "wh_1", acct, 2)

	// Global cutoff is 7 days: on its own this row survives.
	n, err := s.PurgeDeadDeliveries(time.Now(), 7*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("purged %d rows under the global cutoff alone, want 0", n)
	}

	// The tenant says one hour. The two-day-old dead delivery must go.
	if err := s.SetRetentionMaxAge("dev_1", 3600); err != nil {
		t.Fatal(err)
	}
	n, err = s.PurgeDeadDeliveries(time.Now(), 7*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("purged %d rows under a 1h tenant policy, want 1", n)
	}
}

// A live delivery is still retrying and must never be purged, whatever the
// policy says — the payload is the only copy the retry has.
func TestPurgeDeadDeliveriesLeavesLiveRowsAlone(t *testing.T) {
	s := newTestStore(t)
	acct := seedAccount(t, s)
	if err := s.SaveWebhook(model.Webhook{ID: "wh_1", DeveloperID: "dev_1", AccountID: acct, URL: "https://example.com/hook", Events: []string{"*"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveDelivery(Delivery{
		ID: "dl_live", WebhookID: "wh_1", AccountID: acct, EventType: "mail_received",
		Payload: []byte(`{}`), Attempts: 2, Dead: false,
		CreatedAt: time.Now().Add(-30 * 24 * time.Hour).UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetRetentionMaxAge("dev_1", 60); err != nil {
		t.Fatal(err)
	}
	n, err := s.PurgeDeadDeliveries(time.Now(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("purged %d live deliveries, want 0", n)
	}
}
