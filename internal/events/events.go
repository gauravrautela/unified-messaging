// Package events delivers normalized notifications to caller-registered
// webhooks. This is the outbound half of the integration: whatever Graph pushes
// at us, subscribers see one stable event shape.
package events

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/gauravrautela/unified-messaging/internal/accounts"
	"github.com/gauravrautela/unified-messaging/internal/model"
	"github.com/gauravrautela/unified-messaging/internal/store"
)

// DefaultRetrySchedule is the wait before each retry of a failed delivery,
// after the immediate first attempt. Fast early retries absorb a deploy blip;
// the long tail rides out an outage without hammering a dead endpoint. Once
// it is exhausted the delivery is marked dead and kept for inspection.
var DefaultRetrySchedule = []time.Duration{
	30 * time.Second,
	2 * time.Minute,
	10 * time.Minute,
	30 * time.Minute,
	2 * time.Hour,
	6 * time.Hour,
	12 * time.Hour,
}

type Dispatcher struct {
	store  *store.Store
	log    *slog.Logger
	client *http.Client
	queue  chan model.Event
	done   chan struct{}

	// RetrySchedule and RetryPoll are settable before Start.
	RetrySchedule []time.Duration
	RetryPoll     time.Duration
}

func NewDispatcher(s *store.Store, log *slog.Logger) *Dispatcher {
	return &Dispatcher{
		store:  s,
		log:    log.With("component", "events"),
		client: &http.Client{Timeout: 15 * time.Second},
		// Buffered so a slow subscriber never stalls the sync loop. If this
		// fills, events are dropped with a warning rather than blocking sync.
		// Once a delivery has been attempted it lives in the store, so only
		// the first attempt is at risk here.
		queue:         make(chan model.Event, 1024),
		done:          make(chan struct{}),
		RetrySchedule: DefaultRetrySchedule,
		RetryPoll:     30 * time.Second,
	}
}

// Start runs two workers: one drains the in-memory queue of fresh events, the
// other re-sends deliveries that failed earlier. The retry worker owns the
// timing, so a subscriber's outage never backs up fresh deliveries.
func (d *Dispatcher) Start(ctx context.Context) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-ctx.Done():
				return
			case ev := <-d.queue:
				d.deliver(ctx, ev)
			}
		}
	}()
	go func() {
		defer wg.Done()
		t := time.NewTicker(d.RetryPoll)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				d.retryDue(ctx)
			}
		}
	}()
	go func() { wg.Wait(); close(d.done) }()
}

// Wait blocks until the workers have stopped, so shutdown does not cut a
// delivery off mid-flight.
func (d *Dispatcher) Wait() { <-d.done }

func (d *Dispatcher) Emit(ev model.Event) {
	if ev.Timestamp.IsZero() {
		ev.Timestamp = time.Now().UTC()
	}
	select {
	case d.queue <- ev:
	default:
		d.log.Warn("event queue full, dropping", "type", ev.Type, "account_id", ev.AccountID)
	}
}

func (d *Dispatcher) deliver(ctx context.Context, ev model.Event) {
	hooks, err := d.store.ListWebhooksFor(ev.AccountID)
	if err != nil {
		d.log.Error("listing webhooks", "err", err)
		return
	}
	d.log.Debug("dispatching", "event", ev.Type, "account_id", ev.AccountID, "hooks", len(hooks))
	for _, h := range hooks {
		if !subscribes(h, ev.Type) {
			d.log.Debug("hook skipped", "webhook_id", h.ID, "account_id", ev.AccountID,
				"developer_id", h.DeveloperID, "reason", "event filter")
			continue
		}
		// Encoded per hook: the payload names the hook it went through.
		ev.Webhook = &model.WebhookRef{ID: h.ID, Name: h.Name}
		payload, err := json.Marshal(ev)
		if err != nil {
			d.log.Error("encoding event", "err", err)
			return
		}
		dl := store.Delivery{
			WebhookID: h.ID, AccountID: ev.AccountID, EventType: ev.Type,
			Payload: payload, CreatedAt: time.Now().UTC(),
		}
		// Mint the id before the first attempt so every line about this
		// delivery, successful or not, carries the same delivery_id.
		if id, err := accounts.NewID("dl"); err == nil {
			dl.ID = id
		}
		if err := d.post(ctx, h, dl, 1); err != nil {
			d.enqueue(dl, err)
			continue
		}
		d.deliveryLog(dl, h.DeveloperID).Debug("delivery decision",
			"decision", "delivered", "attempts", 1)
	}
}

// enqueue parks a delivery after its first failure.
func (d *Dispatcher) enqueue(dl store.Delivery, cause error) {
	if dl.ID == "" {
		id, err := accounts.NewID("dl")
		if err != nil {
			d.log.Error("minting delivery id", "err", err)
			return
		}
		dl.ID = id
	}
	dl.Attempts = 1
	d.schedule(dl, cause)
}

// schedule records the outcome of a failed attempt and decides when, or
// whether, the next one happens.
func (d *Dispatcher) schedule(dl store.Delivery, cause error) {
	dl.LastError = cause.Error()
	// developer_id is not reachable from a Delivery alone; the rest of the
	// correlation set is.
	log := d.deliveryLog(dl, "")
	// Attempts counts the tries so far; retry N waits RetrySchedule[N-1].
	if dl.Attempts-1 < len(d.RetrySchedule) {
		dl.NextAttemptAt = time.Now().UTC().Add(d.RetrySchedule[dl.Attempts-1])
		log.Debug("delivery decision", "decision", "scheduled retry",
			"next_attempt_at", dl.NextAttemptAt, "attempts", dl.Attempts)
	} else {
		dl.Dead = true
		log.Debug("delivery decision", "decision", "dead", "attempts", dl.Attempts)
		log.Error("webhook delivery abandoned", "attempts", dl.Attempts, "err", cause)
	}
	if err := d.store.SaveDelivery(dl); err != nil {
		d.log.Error("saving delivery for retry", "webhook_id", dl.WebhookID, "err", err)
	}
}

// retryDue re-sends every delivery whose time has come.
func (d *Dispatcher) retryDue(ctx context.Context) {
	due, err := d.store.DueDeliveries(time.Now(), 100)
	if err != nil {
		d.log.Error("listing due deliveries", "err", err)
		return
	}
	// A tick with nothing due is the common case; logging it would drown the
	// DEBUG stream in noise.
	if len(due) > 0 {
		d.log.Debug("retry tick", "due", len(due))
	}
	for _, dl := range due {
		if ctx.Err() != nil {
			return
		}
		h, err := d.store.GetAnyWebhook(dl.WebhookID)
		if err != nil {
			// Hook was removed; the cascade normally handles this, but be safe.
			d.deliveryLog(dl, "").Debug("delivery decision",
				"decision", "dropped", "reason", "webhook gone")
			_ = d.store.DeleteDelivery(dl.ID)
			continue
		}
		dl.Attempts++
		if err := d.post(ctx, h, dl, dl.Attempts); err != nil {
			d.schedule(dl, err)
			continue
		}
		d.deliveryLog(dl, h.DeveloperID).Debug("delivery decision",
			"decision", "delivered", "attempts", dl.Attempts)
		if err := d.store.DeleteDelivery(dl.ID); err != nil {
			d.log.Error("clearing delivered event", "delivery_id", dl.ID, "err", err)
		}
	}
}

func subscribes(h model.Webhook, eventType string) bool {
	if len(h.Events) == 0 {
		return true // no filter means everything
	}
	for _, e := range h.Events {
		if e == eventType || e == "*" {
			return true
		}
	}
	return false
}

// post makes one attempt. Anything but a 2xx is a failure; the caller decides
// what to do about it. Subscribers are told to treat delivery as
// at-least-once and dedupe on (type, email_id).
func (d *Dispatcher) post(ctx context.Context, h model.Webhook, dl store.Delivery, attempt int) error {
	// signed reports only whether a secret exists; the secret itself and the
	// signature derived from it stay out of the log.
	log := d.deliveryLog(dl, h.DeveloperID).With("attempt", attempt)
	log.Debug("delivery attempt", "url", h.URL, "payload_bytes", len(dl.Payload), "signed", h.Secret != "")

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.URL, bytes.NewReader(dl.Payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Outlook-Event", dl.EventType)
	req.Header.Set("X-Outlook-Delivery", fmt.Sprintf("%d", attempt))
	if h.Secret != "" {
		mac := hmac.New(sha256.New, []byte(h.Secret))
		mac.Write(dl.Payload)
		req.Header.Set("X-Outlook-Signature", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	}

	start := time.Now()
	resp, err := d.client.Do(req)
	if err == nil {
		resp.Body.Close()
		log.Debug("delivery response", "status", resp.StatusCode,
			"dur", time.Since(start).Round(time.Millisecond))
		if resp.StatusCode < 300 {
			return nil
		}
		err = fmt.Errorf("status %d", resp.StatusCode)
	} else {
		log.Debug("delivery response", "status", 0,
			"dur", time.Since(start).Round(time.Millisecond), "err", err)
	}
	log.Warn("webhook delivery failed", "url", h.URL, "err", err)
	return err
}

// deliveryLog is the correlation set every line about one delivery carries, so
// a failure and the retries that follow it can be read as one story.
func (d *Dispatcher) deliveryLog(dl store.Delivery, developerID string) *slog.Logger {
	log := d.log.With("delivery_id", dl.ID, "webhook_id", dl.WebhookID,
		"account_id", dl.AccountID, "event", dl.EventType)
	if developerID != "" {
		log = log.With("developer_id", developerID)
	}
	return log
}
