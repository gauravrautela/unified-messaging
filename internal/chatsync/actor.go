package chatsync

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/gauravrautela/unified-messaging/internal/logx"
	"github.com/gauravrautela/unified-messaging/internal/model"
	"github.com/gauravrautela/unified-messaging/internal/provider"
)

// Connection states reported through model.Connection.
const (
	stateConnecting = "connecting"
	stateConnected  = "connected"
	stateBackoff    = "backoff"
	stateStopped    = "stopped"
)

// disconnect is why one connection ended. Exactly one of the three cases
// applies: stop means we are shutting the actor down and must not reconnect,
// loggedOut means the phone removed the linked device and no reconnect can
// ever succeed, and neither set means an ordinary drop worth retrying.
type disconnect struct {
	reason    string
	loggedOut bool
	stop      bool
}

// actor owns one account's connection. Everything that touches the store or
// the dispatcher for this account runs on its single goroutine, reached
// through inbox; mu guards only the health snapshot, which other goroutines
// read.
type actor struct {
	rt     *Runtime
	acct   model.Account
	chat   provider.Chatter
	log    *slog.Logger
	ctx    context.Context
	cancel context.CancelFunc
	inbox  chan func()

	mu         sync.Mutex
	state      string
	since      time.Time
	reconnects int
	failures   int
	lastErr    string
}

func newActor(rt *Runtime, acct model.Account, chat provider.Chatter, parent context.Context) *actor {
	ctx, cancel := context.WithCancel(parent)
	return &actor{
		rt: rt, acct: acct, chat: chat, ctx: ctx, cancel: cancel,
		log:   rt.log.With("account_id", acct.ID, "developer_id", acct.DeveloperID),
		inbox: make(chan func(), 1024),
		state: stateConnecting, since: rt.now(),
	}
}

// stop asks the actor to wind down. Cancelling its context is the single
// signal: it unblocks a backoff sleep, ends the serve loop and tells the sink
// to stop accepting work, all without a second channel to keep in step.
func (a *actor) stop() { a.cancel() }

// run is the connection lifecycle: dial, serve until the socket ends, decide
// whether that ending is worth another attempt. It returns only when the
// account is finished with — stopped, logged out, or out of attempts.
func (a *actor) run() {
	defer a.setState(stateStopped)
	for {
		connID := "conn_" + strings.TrimPrefix(logx.NewRequestID(), "req_")
		log := a.log.With("conn_id", connID)
		// The provider adapter reads its correlation ids off the context and
		// tags its own component, so the context logger carries ids only.
		ctx := logx.With(a.ctx, a.rt.base.With(
			"account_id", a.acct.ID, "developer_id", a.acct.DeveloperID, "conn_id", connID))

		jid, err := a.rt.accts.DeviceJID(a.acct.ID)
		if err != nil {
			log.Warn("no linked device for account", "err", err)
			a.markLoggedOut("no device")
			return
		}

		a.setState(stateConnecting)
		disc := make(chan disconnect, 1)
		conn, err := a.chat.Connect(ctx, a.acct.ID, jid, &sink{a: a, log: log, disc: disc})
		if err != nil {
			n := a.failed(err)
			log.Warn("connect failed", "attempt", n, "err", err)
			// A dead device is not a transient fault: retrying it 30 times only
			// delays telling the end user to relink.
			if errors.Is(err, provider.ErrReauthRequired) {
				a.markLoggedOut("relink required")
				return
			}
			if n >= maxFailures {
				log.Error("giving up on chat account", "attempts", n)
				a.markLoggedOut("unreachable")
				return
			}
			a.setState(stateBackoff)
			if !a.sleep(next(n - 1)) {
				return
			}
			continue
		}

		a.setState(stateConnected)
		log.Info("connected", "reconnects", a.health().Reconnects)
		a.roster(ctx, log)

		// A connection that holds this long has proved itself; forgive the
		// failures that led up to it so an account that flaps once a week
		// never accumulates its way to the cap.
		stable := time.AfterFunc(stableAfter, a.stabilised)
		d := a.serve(disc)
		stable.Stop()
		if err := conn.Close(); err != nil {
			log.Warn("closing connection", "err", err)
		}

		switch {
		case d.stop:
			log.Info("actor stopping", "reason", d.reason)
			return
		case d.loggedOut:
			log.Info("logged out by phone", "reason", d.reason)
			a.markLoggedOut(d.reason)
			return
		}

		n := a.dropped(d.reason)
		log.Info("disconnected, backing off", "reason", d.reason, "attempt", n)
		a.setState(stateBackoff)
		if n >= maxFailures {
			log.Error("giving up on chat account", "attempts", n)
			a.markLoggedOut("unreachable")
			return
		}
		if !a.sleep(next(n - 1)) {
			return
		}
	}
}

// serve drains the sink's work in arrival order until the connection ends.
// This loop being the only consumer of inbox is what gives an account a total
// order over its store writes and emitted events.
func (a *actor) serve(disc <-chan disconnect) disconnect {
	for {
		select {
		case f := <-a.inbox:
			f()
		case d := <-disc:
			return d
		case <-a.ctx.Done():
			return disconnect{stop: true, reason: "shutdown"}
		}
	}
}

// roster refreshes chats, attendees and group membership from the provider's
// own view. It runs once per connection and is best-effort: a failure here
// costs metadata freshness, not messages, so it must never stop the actor.
func (a *actor) roster(ctx context.Context, log *slog.Logger) {
	chats, attendees, members, err := a.chat.Chats(ctx, a.acct.ID)
	if err != nil {
		log.Warn("loading roster", "err", err)
		return
	}
	if len(chats)+len(attendees)+len(members) == 0 {
		return
	}
	// Attendees first, so a chat's members resolve to real people rather than
	// bare ids for the window between the two writes.
	for _, at := range attendees {
		if err := a.rt.store.UpsertAttendee(at, a.acct.ID); err != nil {
			log.Error("storing attendee", "err", err)
		}
	}
	for _, c := range chats {
		if err := a.rt.store.UpsertChat(chatOrExisting(a, c)); err != nil {
			log.Error("storing chat", "chat_id", logChat(c.ID), "err", err)
		}
	}
	byChat := map[string][]model.ChatMember{}
	for _, m := range members {
		byChat[m.ChatID] = append(byChat[m.ChatID], m)
	}
	for chatID, ms := range byChat {
		if err := a.rt.store.ReplaceChatMembers(a.acct.ID, chatID, ms); err != nil {
			log.Error("storing chat members", "chat_id", logChat(chatID), "err", err)
		}
	}
	log.Info("roster loaded", "chats", len(chats), "attendees", len(attendees), "members", len(members))
}

// markLoggedOut ends the account: the manager clears the device and flips the
// status, and we publish the status change ourselves rather than owning the
// manager's global hook, which belongs to whoever wires the process together.
func (a *actor) markLoggedOut(reason string) {
	a.rt.accts.MarkLoggedOut(a.acct.ID, reason)
	acct, err := a.rt.store.GetAnyAccount(a.acct.ID)
	if err != nil {
		a.log.Error("reading account after logout", "err", err)
		return
	}
	a.rt.events.Emit(model.Event{Type: model.EventAccountError, AccountID: acct.ID, Account: &acct})
}

// sleep waits out a backoff interval, reporting false if the actor was asked
// to stop while it waited.
func (a *actor) sleep(d time.Duration) bool {
	a.rt.opts.Sleep(a.ctx, d)
	return a.ctx.Err() == nil
}

func (a *actor) setState(s string) {
	a.mu.Lock()
	if a.state != s {
		a.state, a.since = s, a.rt.now()
	}
	a.mu.Unlock()
}

// failed records a connect attempt that never produced a socket.
func (a *actor) failed(err error) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.failures++
	a.lastErr = err.Error()
	return a.failures
}

// dropped records a live connection that ended and wants retrying.
func (a *actor) dropped(reason string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.reconnects++
	a.failures++
	a.lastErr = reason
	return a.failures
}

func (a *actor) stabilised() {
	a.mu.Lock()
	a.failures = 0
	a.mu.Unlock()
}

func (a *actor) health() model.Connection {
	a.mu.Lock()
	defer a.mu.Unlock()
	return model.Connection{State: a.state, Since: a.since, Reconnects: a.reconnects, LastError: a.lastErr}
}

// logChat is how a chat id may appear in a log. A group id is an opaque
// server-minted string and is safe verbatim; anything else may be a phone
// number wearing a JID, so it is reduced to a correlation handle.
func logChat(id string) string {
	if strings.HasSuffix(id, "@g.us") {
		return id
	}
	return logx.Digest(id)
}

// chatOrExisting fills the gaps in a chat the provider reported thinly. A
// message event often carries only an id, and upserting that as-is would wipe
// the name a roster sync had already resolved.
func chatOrExisting(a *actor, c model.Chat) model.Chat {
	c.AccountID = a.acct.ID
	if c.Name != "" && c.Kind != "" {
		return c
	}
	ex, err := a.rt.store.GetChat(a.acct.ID, c.ID)
	if err != nil {
		return c
	}
	if c.Name == "" {
		c.Name = ex.Name
	}
	if c.Kind == "" {
		c.Kind = ex.Kind
	}
	return c
}
