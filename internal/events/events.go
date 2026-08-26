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
	"sync/atomic"
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

	// dropped counts events discarded because the queue stayed full for
	// EmitBlock. It is the only signal that anything was lost, so it is
	// reported on /healthz as well as logged.
	dropped atomic.Int64

	// RetrySchedule and RetryPoll are settable before Start.
	RetrySchedule []time.Duration
	RetryPoll     time.Duration
	// EmitBlock bounds how long Emit blocks on a full queue before giving up
	// on the event. Settable before Start; tests shorten it.
	EmitBlock time.Duration
}

func NewDispatcher(s *store.Store, log *slog.Logger) *Dispatcher {
	return &Dispatcher{
		store:  s,
		log:    log.With("component", "events"),
		client: &http.Client{Timeout: 15 * time.Second},
		// Buffered so a slow subscriber never stalls the sync loop for the
		// common burst. Once it fills, Emit blocks the producer for EmitBlock
		// (see Emit) and only then drops, counting what it dropped: a delivery
		// becomes durable at its first attempt, so an event dropped here is
		// never written to webhook_deliveries and cannot be replayed.
		queue:         make(chan model.Event, 1024),
		done:          make(chan struct{}),
		RetrySchedule: DefaultRetrySchedule,
		RetryPoll:     30 * time.Second,
		EmitBlock:     5 * time.Second,
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
		// Fresh deliveries run on a context detached from ctx: a POST cancelled
		// mid-flight is rescheduled even when the subscriber had already
		// accepted it, and the client's own 15 s timeout is the real bound on
		// how long one can take. ctx decides when this loop stops, nothing else.
		deliverCtx := context.WithoutCancel(ctx)
		for {
			select {
			case <-ctx.Done():
				d.drain(deliverCtx)
				return
			case ev := <-d.queue:
				d.deliver(deliverCtx, ev)
			}
		}
	}()
	go func() {
		defer wg.Done()
		t := time.NewTicker(d.RetryPoll)
		defer t.Stop()
		// Retries post on the same non-cancelled context as fresh deliveries:
		// ctx only decides when this loop stops, never whether an accepted
		// POST is cut off mid-flight.
		retryCtx := context.WithoutCancel(ctx)
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				d.retryDue(ctx, retryCtx)
			}
		}
	}()
	go func() { wg.Wait(); close(d.done) }()
}

// Wait blocks until the workers have stopped, so shutdown does not cut a
// delivery off mid-flight.
func (d *Dispatcher) Wait() { <-d.done }

// Dropped reports how many events have been discarded because the queue was
// full for longer than EmitBlock. Non-zero means webhook subscribers missed
// notifications that were never persisted and cannot be replayed.
func (d *Dispatcher) Dropped() int64 { return d.dropped.Load() }

func (d *Dispatcher) Emit(ev model.Event) {
	if ev.Timestamp.IsZero() {
		ev.Timestamp = time.Now().UTC()
	}
	// Fast path: the queue almost always has room, and an emitter must not pay
	// for a timer it will not use.
	select {
	case d.queue <- ev:
		return
	default:
	}
	// Full queue: push back on the emitter rather than discard. The producers
	// are per-account actors whose events the provider replays on reconnect and
	// whose handlers are idempotent, so blocking them is the spec's
	// back-pressure rule. The bound is what keeps an API request that emits
	// inline from hanging on a stuck subscriber forever.
	t := time.NewTimer(d.EmitBlock)
	defer t.Stop()
	select {
	case d.queue <- ev:
	case <-t.C:
		n := d.dropped.Add(1)
		d.log.Warn("event queue full, dropping", "type", ev.Type, "account_id", ev.AccountID,
			"dropped_total", n)
	}
}

// drainDeadline bounds the shutdown drain: whatever is already queued gets one
// attempt, but a stuck subscriber must not hold the process open past the
// platform's own termination grace period.
const drainDeadline = 5 * time.Second

// drain makes a best-effort first attempt at everything still queued when the
// dispatcher is cancelled. Without it, Wait() only looked like the spec's
// "dispatcher drain": a first attempt is what makes a delivery durable, so an
// event discarded here is lost for good rather than retried from the store.
//
// ctx here is the delivery worker's own detached context, not the cancelled
// one: requests built from the cancelled context would abort mid-flight and be
// rescheduled even where the subscriber had already accepted them.
func (d *Dispatcher) drain(ctx context.Context) {
	if len(d.queue) == 0 {
		return
	}
	drainCtx, cancel := context.WithTimeout(ctx, drainDeadline)
	defer cancel()
	n := 0
	for {
		select {
		case ev := <-d.queue:
			d.deliver(drainCtx, ev)
			n++
			if drainCtx.Err() != nil {
				d.log.Warn("shutdown drain cut short", "delivered", n, "remaining", len(d.queue))
				return
			}
		default:
			d.log.Info("shutdown drain complete", "delivered", n)
			return
		}
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
// retryDue re-attempts every due delivery. stop ends the loop between
// deliveries (shutdown); postCtx is what each POST runs under, so one that a
// subscriber has already accepted is never aborted mid-flight.
func (d *Dispatcher) retryDue(stop, postCtx context.Context) {
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
		if stop.Err() != nil {
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
		if err := d.post(postCtx, h, dl, dl.Attempts); err != nil {
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
