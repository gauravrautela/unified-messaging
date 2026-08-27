package chatsync

import (
	"log/slog"
	"time"

	"github.com/gauravrautela/unified-messaging/internal/logx"
	"github.com/gauravrautela/unified-messaging/internal/model"
	"github.com/gauravrautela/unified-messaging/internal/provider"
)

// sink is one connection's inbound edge. The provider calls it from whatever
// goroutine its library happens to own, so every method does nothing but hand
// a closure to the actor: the store write and the event that follows it run on
// the actor's goroutine, in the order the provider reported them.
type sink struct {
	a    *actor
	log  *slog.Logger
	disc chan<- disconnect
}

var _ provider.EventSink = (*sink)(nil)

// foreignAccount reports whether accountID is not the account this sink was
// created for, logging the mismatch first. The adapter is trusted to report
// its own connection's events, but never so trusted that a bug — or, worse,
// an adapter confusing two live sockets — can land another tenant's message
// in this account's mirror: every callback checks the accountID it was
// actually called with, rather than assuming it must match the one closed
// over in s.a. Digested, not logged raw: an account id can double as an
// identifier worth protecting the same way a chat id is.
func (s *sink) foreignAccount(accountID string) bool {
	if accountID == s.a.acct.ID {
		return false
	}
	s.a.log.Error("chat sink: foreign account id",
		"got", logx.Digest(accountID), "want", logx.Digest(s.a.acct.ID))
	return true
}

// enqueue hands work to the actor. A full inbox blocks the caller rather than
// dropping the event — back-pressure onto the provider's reader is recoverable,
// a silently discarded message is not — but never past the point where the
// actor has stopped draining.
func (s *sink) enqueue(f func()) {
	select {
	case s.a.inbox <- f:
		return
	default:
	}
	select {
	case s.a.inbox <- f:
	case <-s.a.ctx.Done():
		s.log.Warn("dropping chat event; actor is stopping")
	}
}

func (s *sink) Message(accountID string, m model.ChatMessage, chat model.Chat, sender model.Attendee) {
	if s.foreignAccount(accountID) {
		return
	}
	s.enqueue(func() {
		m.AccountID = accountID
		log := s.log.With("chat_id", logChat(m.ChatID), "message_id", m.ID)
		if err := s.a.rt.store.UpsertAttendee(sender, accountID); err != nil {
			log.Error("storing sender", "err", err)
		}
		// Own messages create a chat too: a conversation started from the phone
		// would otherwise have no row until the other side replied. The
		// provider is expected to leave Name empty when it cannot trust it —
		// on an own-message echo, for instance — and chatOrExisting keeps
		// whatever a roster sync already resolved.
		if err := s.a.rt.store.UpsertChat(chatOrExisting(s.a, chat)); err != nil {
			log.Error("storing chat", "err", err)
		}
		inserted, err := s.a.rt.store.UpsertChatMessage(m)
		if err != nil {
			log.Error("storing message", "err", err)
			return
		}
		// A message we already hold is either a reconnect replay or the echo of
		// a send the API layer already recorded. Either way the row is right
		// and a second event would be a duplicate to every subscriber.
		switch {
		case !inserted && m.IsFromMe:
			log.Debug("chat event", "kind", "message", "decision", "own-echo", "text_bytes", len(m.Text))
			return
		case !inserted:
			log.Debug("chat event", "kind", "message", "decision", "replay", "text_bytes", len(m.Text))
			return
		}
		delta := 1
		if m.IsFromMe {
			delta = 0
		}
		if err := s.a.rt.store.BumpChat(accountID, m.ChatID, m.SentAt, delta); err != nil {
			log.Error("bumping chat", "err", err)
		}
		typ := model.EventChatReceived
		if m.IsFromMe {
			typ = model.EventChatSent
		}
		log.Debug("chat event", "kind", "message", "decision", "new",
			"from_me", m.IsFromMe, "text_bytes", len(m.Text), "event", typ)
		ev := model.Event{Type: typ, AccountID: accountID}
		if full, err := s.a.rt.store.GetChatMessage(accountID, m.ID); err == nil {
			ev.Message = &full
		}
		if c, err := s.a.rt.store.GetChat(accountID, m.ChatID); err == nil {
			ev.Chat = &c
		}
		s.a.rt.events.Emit(ev)
	})
}

func (s *sink) Receipt(accountID, chatID string, messageIDs []string, status string) {
	if s.foreignAccount(accountID) {
		return
	}
	ids := append([]string(nil), messageIDs...)
	s.enqueue(func() {
		if len(ids) == 0 {
			return
		}
		log := s.log.With("chat_id", logChat(chatID))
		if err := s.a.rt.store.SetMessageStatus(accountID, ids, status); err != nil {
			log.Error("storing receipt", "err", err)
			return
		}
		// A read receipt is the phone telling us the human has seen the chat.
		// Which ids it names is beside the point: the unread counter tracks the
		// conversation, not the individual messages, and the ones we do not own
		// are exactly the ones that made it non-zero.
		if status == "read" {
			if err := s.a.rt.store.ClearUnread(accountID, chatID); err != nil {
				log.Error("clearing unread", "err", err)
			}
		}
		log.Debug("chat event", "kind", "receipt", "decision", "applied", "status", status, "ids", len(ids))
		s.a.rt.events.Emit(model.Event{Type: model.EventChatUpdated, AccountID: accountID,
			MessageIDs: ids, Status: status})
	})
}

func (s *sink) Reaction(accountID, chatID, messageID string, r model.Reaction) {
	if s.foreignAccount(accountID) {
		return
	}
	s.enqueue(func() {
		log := s.log.With("chat_id", logChat(chatID), "message_id", messageID)
		if err := s.a.rt.store.ApplyReaction(accountID, messageID, r); err != nil {
			// Commonly a reaction to a message from before this account was
			// connected: there is nothing to attach it to and nothing to fix.
			log.Warn("applying reaction", "err", err)
			return
		}
		log.Debug("chat event", "kind", "reaction", "decision", "applied", "removed", r.Emoji == "")
		s.a.rt.events.Emit(model.Event{Type: model.EventChatReaction, AccountID: accountID,
			MessageIDs: []string{messageID}, Reaction: &r})
	})
}

func (s *sink) Edited(accountID, chatID, messageID, text string, at time.Time) {
	if s.foreignAccount(accountID) {
		return
	}
	s.enqueue(func() {
		log := s.log.With("chat_id", logChat(chatID), "message_id", messageID)
		prev, err := s.a.rt.store.GetChatMessage(accountID, messageID)
		if err != nil {
			log.Debug("chat event", "kind", "edit", "decision", "unknown-message")
			return
		}
		if prev.EditedAt != nil && prev.Text == text {
			log.Debug("chat event", "kind", "edit", "decision", "replay", "text_bytes", len(text))
			return
		}
		if err := s.a.rt.store.EditChatMessage(accountID, messageID, text, at); err != nil {
			log.Error("storing edit", "err", err)
			return
		}
		full, err := s.a.rt.store.GetChatMessage(accountID, messageID)
		if err != nil {
			log.Error("reading edited message", "err", err)
			return
		}
		log.Debug("chat event", "kind", "edit", "decision", "applied", "text_bytes", len(text))
		s.a.rt.events.Emit(model.Event{Type: model.EventChatUpdated, AccountID: accountID,
			MessageIDs: []string{messageID}, Change: "edited", Message: &full})
	})
}

func (s *sink) Deleted(accountID, chatID, messageID string) {
	if s.foreignAccount(accountID) {
		return
	}
	s.enqueue(func() {
		log := s.log.With("chat_id", logChat(chatID), "message_id", messageID)
		prev, err := s.a.rt.store.GetChatMessage(accountID, messageID)
		if err != nil {
			log.Debug("chat event", "kind", "delete", "decision", "unknown-message")
			return
		}
		if prev.Deleted {
			log.Debug("chat event", "kind", "delete", "decision", "replay")
			return
		}
		if err := s.a.rt.store.RevokeChatMessage(accountID, messageID); err != nil {
			log.Error("revoking message", "err", err)
			return
		}
		log.Debug("chat event", "kind", "delete", "decision", "applied")
		s.a.rt.events.Emit(model.Event{Type: model.EventChatDeleted, AccountID: accountID,
			MessageIDs: []string{messageID}})
	})
}

// Disconnected does not go through the inbox: it is the reason the inbox is
// about to stop being drained, so queueing it behind the work it cancels would
// deadlock a full inbox. The channel is one-deep and the send never blocks —
// a second report of the same dead socket tells us nothing new.
func (s *sink) Disconnected(accountID, reason string, loggedOut bool) {
	if s.foreignAccount(accountID) {
		return
	}
	select {
	case s.disc <- disconnect{reason: reason, loggedOut: loggedOut}:
	default:
		s.log.Debug("duplicate disconnect ignored", "logged_out", loggedOut)
	}
}
