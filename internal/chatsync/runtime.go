// Package chatsync keeps one live connection per chat account and turns the
// provider's events into stored rows and outbound webhooks. It is the chat
// counterpart of the syncer: where mail is pulled on cursors, chat is pushed
// over a socket the provider holds open.
//
// The shape is a supervisor over one actor per account. An actor owns exactly
// one goroutine, so every store write and every emitted event for an account
// happens in the order the provider reported it — the provider's own callbacks
// arrive on whatever goroutine its library chooses, and the actor's inbox is
// what serialises them again.
package chatsync

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/gauravrautela/unified-messaging/internal/accounts"
	"github.com/gauravrautela/unified-messaging/internal/events"
	"github.com/gauravrautela/unified-messaging/internal/model"
	"github.com/gauravrautela/unified-messaging/internal/provider"
	"github.com/gauravrautela/unified-messaging/internal/store"
)

// ErrCapacity means this process already holds as many live sockets as it is
// configured for. Chat connections are long-lived and each costs a goroutine
// and a socket, so the limit is a real one rather than a formality.
var ErrCapacity = errors.New("chatsync: account capacity reached")

type Options struct {
	MaxAccounts int
	// Clock and Sleep are test hooks. Nil Clock means time.Now; nil Sleep
	// means a context-aware wait.
	Clock func() time.Time
	Sleep func(context.Context, time.Duration)
}

type Runtime struct {
	store  *store.Store
	reg    *provider.Registry
	accts  *accounts.Manager
	events *events.Dispatcher
	// log carries component=chatsync for this package's own lines; base is the
	// untagged logger that goes into a context, because a context logger holds
	// correlation ids only and each package tags its own component at the leaf.
	log  *slog.Logger
	base *slog.Logger
	opts Options

	mu      sync.Mutex
	actors  map[string]*actor
	ctx     context.Context
	started bool
	wg      sync.WaitGroup
}

func New(s *store.Store, reg *provider.Registry, a *accounts.Manager, d *events.Dispatcher, log *slog.Logger, opts Options) *Runtime {
	if opts.MaxAccounts <= 0 {
		opts.MaxAccounts = 200
	}
	if opts.Sleep == nil {
		opts.Sleep = func(ctx context.Context, d time.Duration) {
			t := time.NewTimer(d)
			defer t.Stop()
			select {
			case <-ctx.Done():
			case <-t.C:
			}
		}
	}
	return &Runtime{
		store: s, reg: reg, accts: a, events: d,
		log: log.With("component", "chatsync"), base: log, opts: opts,
		actors: map[string]*actor{},
		ctx:    context.Background(),
	}
}

// Start attaches every live chat account. It is safe to call again: a re-scan
// attaches only the accounts that have no actor yet, and the context of the
// first call is the one that governs shutdown for every actor.
func (r *Runtime) Start(ctx context.Context) {
	r.mu.Lock()
	if !r.started {
		r.ctx, r.started = ctx, true
	}
	r.mu.Unlock()

	accts, err := r.store.ListChatAccounts()
	if err != nil {
		r.log.Error("listing chat accounts", "err", err)
		return
	}
	r.log.Info("attaching chat accounts", "accounts", len(accts), "max", r.opts.MaxAccounts)
	for _, a := range accts {
		if err := r.Attach(a.ID); err != nil {
			r.log.Warn("attach at start", "account_id", a.ID, "developer_id", a.DeveloperID, "err", err)
		}
	}
}

// Attach starts an actor for accountID. Attaching an already-attached account
// is a no-op, which is what makes Start idempotent.
func (r *Runtime) Attach(accountID string) error {
	// A link that finishes during shutdown lands here after Wait() has already
	// returned. Starting an actor then would wg.Add(1) on a WaitGroup a Wait
	// has completed on — the reuse hazard the docs warn about — for a
	// connection that is about to be torn down anyway.
	r.mu.Lock()
	runCtx := r.ctx
	r.mu.Unlock()
	// nil means Start has not run yet, which is not shutdown: Attach still
	// works, exactly as it did before this guard.
	if runCtx != nil {
		if err := runCtx.Err(); err != nil {
			return err
		}
	}
	acct, err := r.store.GetAnyAccount(accountID)
	if err != nil {
		return err
	}
	p, err := r.reg.Get(acct.Provider)
	if err != nil {
		return err
	}
	chatter := p.Chat()
	if chatter == nil {
		return fmt.Errorf("chatsync: %s is not a chat provider", acct.Provider)
	}

	r.mu.Lock()
	if ex, ok := r.actors[accountID]; ok {
		if !ex.isTerminating() {
			r.mu.Unlock()
			return nil
		}
		// The entry belongs to an actor that has already given the account up
		// and is only finishing its bookkeeping. Evict it here rather than wait
		// for it: a relink arriving in that window must get a live connection,
		// not a silent no-op. The old actor's own remove is identity-checked,
		// so it cannot evict the replacement we are about to install.
		delete(r.actors, accountID)
	}
	if r.liveCount() >= r.opts.MaxAccounts {
		r.mu.Unlock()
		return ErrCapacity
	}
	a := newActor(r, acct, chatter, r.ctx)
	r.actors[accountID] = a
	r.wg.Add(1)
	r.mu.Unlock()

	go func() {
		defer r.wg.Done()
		a.run()
		r.remove(accountID, a)
	}()
	return nil
}

// Detach stops an account's actor and drops it. The slot is freed before the
// goroutine has finished unwinding, so a caller that detaches to make room can
// attach again immediately.
func (r *Runtime) Detach(accountID string) {
	r.mu.Lock()
	a := r.actors[accountID]
	if a != nil {
		delete(r.actors, accountID)
	}
	r.mu.Unlock()
	if a != nil {
		a.stop()
	}
}

// remove clears the map entry only if it still points at this actor, so a
// slower exiting actor cannot evict its own replacement.
func (r *Runtime) remove(accountID string, a *actor) {
	r.mu.Lock()
	if r.actors[accountID] == a {
		delete(r.actors, accountID)
	}
	r.mu.Unlock()
}

// HealthFor reports an account's live connection state. ok is false when this
// process holds no actor for it — a mail account, or one that stopped.
func (r *Runtime) HealthFor(accountID string) (model.Connection, bool) {
	r.mu.Lock()
	a := r.actors[accountID]
	r.mu.Unlock()
	if a == nil {
		return model.Connection{}, false
	}
	return a.health(), true
}

// Count is how many accounts this process is actually serving. Actors that
// have given their account up are excluded, so a run of logouts never eats the
// capacity that the accounts replacing them need.
func (r *Runtime) Count() int { r.mu.Lock(); defer r.mu.Unlock(); return r.liveCount() }

// liveCount counts actors still serving their account. Callers hold r.mu.
func (r *Runtime) liveCount() int {
	n := 0
	for _, a := range r.actors {
		if !a.isTerminating() {
			n++
		}
	}
	return n
}

// Max is the configured connection ceiling, reported so an operator can see
// how close to it the process is running.
func (r *Runtime) Max() int { return r.opts.MaxAccounts }

// Wait blocks until every actor has stopped, so shutdown does not cut a store
// write off mid-flight.
func (r *Runtime) Wait() { r.wg.Wait() }

func (r *Runtime) now() time.Time {
	if r.opts.Clock != nil {
		return r.opts.Clock()
	}
	return time.Now().UTC()
}
