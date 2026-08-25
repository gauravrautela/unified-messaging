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
		log:    log,
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
	for _, h := range hooks {
		if !subscribes(h, ev.Type) {
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
		if err := d.post(ctx, h, dl, 1); err != nil {
			d.enqueue(dl, err)
		}
	}
}

// enqueue parks a delivery after its first failure.
func (d *Dispatcher) enqueue(dl store.Delivery, cause error) {
	id, err := accounts.NewID("dl")
	if err != nil {
		d.log.Error("minting delivery id", "err", err)
		return
	}
	dl.ID = id
	dl.Attempts = 1
	d.schedule(dl, cause)
}

// schedule records the outcome of a failed attempt and decides when, or
// whether, the next one happens.
func (d *Dispatcher) schedule(dl store.Delivery, cause error) {
	dl.LastError = cause.Error()
	// Attempts counts the tries so far; retry N waits RetrySchedule[N-1].
	if dl.Attempts-1 < len(d.RetrySchedule) {
		dl.NextAttemptAt = time.Now().UTC().Add(d.RetrySchedule[dl.Attempts-1])
	} else {
		dl.Dead = true
		d.log.Error("webhook delivery abandoned",
			"webhook_id", dl.WebhookID, "event", dl.EventType, "attempts", dl.Attempts, "err", cause)
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
	for _, dl := range due {
		if ctx.Err() != nil {
			return
		}
		h, err := d.store.GetAnyWebhook(dl.WebhookID)
		if err != nil {
			// Hook was removed; the cascade normally handles this, but be safe.
			_ = d.store.DeleteDelivery(dl.ID)
			continue
		}
		dl.Attempts++
		if err := d.post(ctx, h, dl, dl.Attempts); err != nil {
			d.schedule(dl, err)
			continue
		}
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

	resp, err := d.client.Do(req)
	if err == nil {
		resp.Body.Close()
		if resp.StatusCode < 300 {
			return nil
		}
		err = fmt.Errorf("status %d", resp.StatusCode)
	}
	d.log.Warn("webhook delivery failed",
		"webhook_id", h.ID, "url", h.URL, "attempt", attempt, "err", err)
	return err
}
