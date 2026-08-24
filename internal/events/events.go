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
	"time"

	"github.com/gauravrautela/unified-messaging/internal/model"
	"github.com/gauravrautela/unified-messaging/internal/store"
)

type Dispatcher struct {
	store  *store.Store
	log    *slog.Logger
	client *http.Client
	queue  chan model.Event
	done   chan struct{}
}

func NewDispatcher(s *store.Store, log *slog.Logger) *Dispatcher {
	return &Dispatcher{
		store:  s,
		log:    log,
		client: &http.Client{Timeout: 15 * time.Second},
		// Buffered so a slow subscriber never stalls the sync loop. If this
		// fills, events are dropped with a warning rather than blocking sync —
		// a real system would spill to a durable queue instead.
		queue: make(chan model.Event, 1024),
		done:  make(chan struct{}),
	}
}

func (d *Dispatcher) Start(ctx context.Context) {
	go func() {
		defer close(d.done)
		for {
			select {
			case <-ctx.Done():
				return
			case ev := <-d.queue:
				d.deliver(ctx, ev)
			}
		}
	}()
}

// Wait blocks until the worker has stopped, so shutdown does not cut a delivery
// off mid-flight.
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
	hooks, err := d.store.ListWebhooks()
	if err != nil {
		d.log.Error("listing webhooks", "err", err)
		return
	}
	payload, err := json.Marshal(ev)
	if err != nil {
		d.log.Error("encoding event", "err", err)
		return
	}

	for _, h := range hooks {
		if !subscribes(h, ev.Type) {
			continue
		}
		d.post(ctx, h, ev.Type, payload)
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

// post retries a few times with backoff. Subscribers are told to treat delivery
// as at-least-once and dedupe on (type, email_id).
func (d *Dispatcher) post(ctx context.Context, h model.Webhook, eventType string, payload []byte) {
	const attempts = 3
	for attempt := 1; attempt <= attempts; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.URL, bytes.NewReader(payload))
		if err != nil {
			d.log.Error("building webhook request", "webhook_id", h.ID, "err", err)
			return
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Outlook-Event", eventType)
		req.Header.Set("X-Outlook-Delivery", fmt.Sprintf("%d", attempt))
		if h.Secret != "" {
			mac := hmac.New(sha256.New, []byte(h.Secret))
			mac.Write(payload)
			req.Header.Set("X-Outlook-Signature", "sha256="+hex.EncodeToString(mac.Sum(nil)))
		}

		resp, err := d.client.Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode < 300 {
				return
			}
			err = fmt.Errorf("status %d", resp.StatusCode)
		}
		d.log.Warn("webhook delivery failed",
			"webhook_id", h.ID, "url", h.URL, "attempt", attempt, "err", err)

		if attempt == attempts {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Duration(attempt*attempt) * time.Second):
		}
	}
}
