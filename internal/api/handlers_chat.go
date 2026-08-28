package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gauravrautela/unified-messaging/internal/accounts"
	"github.com/gauravrautela/unified-messaging/internal/model"
	"github.com/gauravrautela/unified-messaging/internal/provider"
	"github.com/gauravrautela/unified-messaging/internal/store"
)

// resolveChatAccount is the chat-route counterpart to resolve/resolveID: it
// shares resolveAccount's ownership check (never resolveID itself — that one
// now enforces the mail capability and would 400 every chat account before
// this function got a chance to run), then insists the account's provider is
// a chat one.
func (s *Server) resolveChatAccount(w http.ResponseWriter, r *http.Request, id string) (model.Account, provider.Chatter, bool) {
	acct, p, ok := s.resolveAccount(w, r, id)
	if !ok {
		return model.Account{}, nil, false
	}
	chatter := p.Chat()
	if chatter == nil {
		writeError(w, http.StatusBadRequest, "unsupported_for_kind", "this account does not support chat")
		return model.Account{}, nil, false
	}
	return acct, chatter, true
}

// selfAttendee resolves the account's own attendee row, falling back to a bare
// stand-in when the roster sync has not populated one yet (e.g. straight after
// linking, before the first Chats() pull completes).
//
// The fallback deliberately carries no id. acct.Identifier is an E.164 phone
// (+15551234567), not the …@s.whatsapp.net JID the roster keys attendees by, so
// recording it as sender_id produced a message whose sender resolved to nothing
// on GET /api/v1/attendees/{id}. An empty sender id is honest: the message is
// still marked is_from_me, and the roster fills the sender in as soon as it
// arrives.
func (s *Server) selfAttendee(acct model.Account) (model.Attendee, error) {
	self, err := s.store.SelfAttendee(acct.ID)
	if errors.Is(err, store.ErrNotFound) {
		return model.Attendee{IsSelf: true}, nil
	}
	return self, err
}

func apiErr(code, msg string) any {
	var e apiError
	e.Error.Code = code
	e.Error.Message = msg
	return e
}

// ---- raw body capture ----
//
// A handler that needs Idempotency-Key support reads the body once, up
// front, into a byte slice — both to decode its own request struct from and
// to hand to withIdempotency for hashing. The bytes ride the request context
// rather than an extra parameter so the call site can stay
// s.withIdempotency(w, r, dev.ID, acct.ID, do).

type rawBodyCtxKey struct{}

// rawBodyLimit bounds a chat send: text messages have no legitimate reason
// to approach even this, let alone the old 32 MB ceiling.
const rawBodyLimit = 64 << 10

func readRawBody(r *http.Request) (*http.Request, []byte, error) {
	if r.Body == nil {
		return r, nil, nil
	}
	// Read one byte past the limit so an over-size body is detected here
	// rather than silently truncated: exactly limit+1 bytes back means the
	// body was larger than allowed.
	raw, err := io.ReadAll(io.LimitReader(r.Body, rawBodyLimit+1))
	if err != nil {
		return r, nil, err
	}
	if len(raw) > rawBodyLimit {
		return r, nil, errBodyTooLarge
	}
	r.Body = io.NopCloser(bytes.NewReader(raw))
	return r.WithContext(context.WithValue(r.Context(), rawBodyCtxKey{}, raw)), raw, nil
}

func rawBodyFrom(ctx context.Context) []byte {
	b, _ := ctx.Value(rawBodyCtxKey{}).([]byte)
	return b
}

// idempotencyRecord is what PutIdempotency/GetIdempotency store: enough to
// detect a same-key-different-body replay and to reproduce the original
// response byte-for-byte.
type idempotencyRecord struct {
	Hash   string          `json:"hash"`
	Status int             `json:"status"`
	Body   json.RawMessage `json:"body"`
}

// operationHash scopes an Idempotency-Key to the specific operation it was
// sent with: method, path, account and body. Hashing the raw body alone
// would let the same key replay across two different chats (account_id often
// rides the query string or a path segment, not the JSON body a message-send
// route carries) or even two different accounts — a client bug that must be
// rejected as a conflict, not silently served the wrong chat's cached reply.
func operationHash(r *http.Request, acctID string) string {
	h := sha256.New()
	h.Write([]byte(r.Method))
	h.Write([]byte{'\n'})
	h.Write([]byte(r.URL.Path))
	h.Write([]byte{'\n'})
	h.Write([]byte(acctID))
	h.Write([]byte{'\n'})
	h.Write(rawBodyFrom(r.Context()))
	return hex.EncodeToString(h.Sum(nil))
}

// withIdempotency runs do exactly once per (developer, Idempotency-Key,
// operation) triple. No header at all means "not idempotent": do runs every
// time. A header seen before, with a matching operation hash, replays the
// stored response without calling do again; a hash mismatch under the same
// key is a client bug and gets 409 idempotency_conflict.
//
// Concurrent callers with the same key race on ReserveIdempotency, which is
// a single atomic INSERT ... ON CONFLICT DO NOTHING: exactly one of them
// "wins" and runs do; every loser goes to replayOrConflict, which either
// replays the winner's completed response, reports the conflict, or — if it
// arrives before the winner has finished — reports 409 "in progress" rather
// than running the operation a second time. A losing operation deletes its
// reservation instead of completing it, so a genuine retry after a failure
// is not locked out forever. Keys are purged lazily (anything older than
// 24h) on every successful write, so the table never grows unbounded
// without a separate sweep job.
func (s *Server) withIdempotency(w http.ResponseWriter, r *http.Request, devID, acctID string, do func() (int, any)) {
	key := r.Header.Get("Idempotency-Key")
	if key == "" {
		status, body := do()
		writeJSON(w, status, body)
		return
	}
	hash := operationHash(r, acctID)

	won, err := s.store.ReserveIdempotency(devID, key)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if !won {
		s.replayOrConflict(w, devID, key, hash)
		return
	}

	status, body := do()
	bodyJSON, err := json.Marshal(body)
	if err != nil {
		_ = s.store.DeleteIdempotency(devID, key)
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if status >= 200 && status < 300 {
		if recJSON, err := json.Marshal(idempotencyRecord{Hash: hash, Status: status, Body: bodyJSON}); err == nil {
			s.store.PurgeIdempotency(time.Now().Add(-24 * time.Hour))
			_ = s.store.PutIdempotency(devID, key, recJSON)
		}
	} else {
		// The operation failed: release the reservation so a client that
		// retries with the same key (the whole point of sending one) gets to
		// try again instead of being told "in progress" indefinitely.
		_ = s.store.DeleteIdempotency(devID, key)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(bodyJSON)
}

// replayOrConflict handles every caller that lost the ReserveIdempotency
// race: the key belongs to someone else's request, either still running
// (an empty placeholder response) or already finished (a real one).
func (s *Server) replayOrConflict(w http.ResponseWriter, devID, key, hash string) {
	stored, err := s.store.GetIdempotency(devID, key)
	if err != nil || len(stored) == 0 {
		// Either the reservation is still a placeholder (winner hasn't
		// finished do() yet) or it just vanished — the winner's operation
		// failed and released it in the instant between our lost reservation
		// and this read. Both are transient "try again shortly", not a
		// permanent conflict, but a 409 here is the safe answer either way:
		// it never re-runs the operation and never fabricates a response.
		writeError(w, http.StatusConflict, "idempotency_conflict", "a request with this key is in progress")
		return
	}
	var rec idempotencyRecord
	if json.Unmarshal(stored, &rec) != nil || rec.Status == 0 {
		writeError(w, http.StatusConflict, "idempotency_conflict", "a request with this key is in progress")
		return
	}
	if rec.Hash != hash {
		writeError(w, http.StatusConflict, "idempotency_conflict",
			"this Idempotency-Key was already used with a different request")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(rec.Status)
	_, _ = w.Write(rec.Body)
}

// ---- chats ----

func (s *Server) handleListChats(w http.ResponseWriter, r *http.Request) {
	acct, _, ok := s.resolveChatAccount(w, r, r.URL.Query().Get("account_id"))
	if !ok {
		return
	}
	q := r.URL.Query()
	limit, offset := paging(r)
	query := store.ChatQuery{AccountID: acct.ID, Kind: q.Get("kind"), Search: q.Get("q"), Limit: limit, Offset: offset}
	if v := q.Get("unread"); v != "" {
		unread := v == "true" || v == "1"
		query.Unread = &unread
	}
	chats, err := s.store.ListChats(query)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, listResponse[model.Chat]{Items: chats, Limit: limit, Offset: offset})
}

// startChatRequest is the body of POST /api/v1/chats. account_id rides in
// the body (there is no {id} in the path yet — the whole point is to create
// one), unlike every other chat route.
type startChatRequest struct {
	AccountID  string `json:"account_id"`
	Phone      string `json:"phone,omitempty"`
	AttendeeID string `json:"attendee_id,omitempty"`
	Text       string `json:"text"`
}

func (s *Server) handleStartChat(w http.ResponseWriter, r *http.Request) {
	r, raw, err := readRawBody(r)
	if err != nil {
		writeDecodeError(w, err)
		return
	}
	var req startChatRequest
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body", err.Error())
		return
	}
	acct, chatter, ok := s.resolveChatAccount(w, r, req.AccountID)
	if !ok {
		return
	}
	if strings.TrimSpace(req.Text) == "" {
		writeError(w, http.StatusBadRequest, "missing_text", "text is required")
		return
	}
	if req.Phone == "" && req.AttendeeID == "" {
		writeError(w, http.StatusBadRequest, "missing_recipient", "phone or attendee_id is required")
		return
	}

	dev, _ := developerFrom(r.Context())
	s.withIdempotency(w, r, dev.ID, acct.ID, func() (int, any) {
		phone := req.Phone
		haveExisting := false
		if req.AttendeeID != "" {
			att, err := s.store.GetAttendee(acct.ID, req.AttendeeID)
			if err != nil {
				if errors.Is(err, store.ErrNotFound) {
					return http.StatusNotFound, apiErr("not_found", "no such attendee")
				}
				return http.StatusInternalServerError, apiErr("internal", err.Error())
			}
			phone = att.Phone
			haveExisting = true
		}

		chatID, err := chatter.StartDirect(r.Context(), acct.ID, phone)
		if err != nil {
			// Only the not-connected case is remapped. StartDirect's other
			// ErrNotFound — "this phone is not on WhatsApp" — deliberately stays
			// a 502: a 404 here would be indistinguishable from the 404 this API
			// uses for "belongs to another developer".
			if errors.Is(err, provider.ErrNotConnected) {
				return providerError(err)
			}
			return http.StatusBadGateway, apiErr("provider_error", err.Error())
		}
		if err := s.store.UpsertChat(model.Chat{ID: chatID, AccountID: acct.ID, Kind: "direct"}); err != nil {
			return http.StatusInternalServerError, apiErr("internal", err.Error())
		}
		switch {
		case haveExisting:
			// Nothing to write: phone was read from this very attendee a few
			// lines up, so there is no new fact to record, and UpsertAttendee is
			// a full profile overwrite rather than a merge — writing back a bare
			// {id, phone} would blank the name and is_self we already have.
		default:
			// No attendee_id was given: phone alone identified the recipient,
			// so there is no existing profile to protect and chatID doubles
			// as a serviceable attendee id for a brand-new direct chat.
			if err := s.store.UpsertAttendee(model.Attendee{ID: chatID, Phone: phone}, acct.ID); err != nil {
				return http.StatusInternalServerError, apiErr("internal", err.Error())
			}
		}

		full, status, apierr := s.sendChatText(r.Context(), acct, chatter, chatID, req.Text, "")
		if apierr != nil {
			return status, apierr
		}
		c, err := s.store.GetChat(acct.ID, chatID)
		if err != nil {
			return http.StatusInternalServerError, apiErr("internal", err.Error())
		}
		s.dispatcher.Emit(model.Event{Type: model.EventChatSent, AccountID: acct.ID, Message: &full, Chat: &c})
		return http.StatusCreated, map[string]any{"chat": c, "message": full}
	})
}

func (s *Server) handleGetChat(w http.ResponseWriter, r *http.Request) {
	acct, _, ok := s.resolveChatAccount(w, r, r.URL.Query().Get("account_id"))
	if !ok {
		return
	}
	chat, err := s.store.GetChat(acct.ID, r.PathValue("id"))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "no such chat")
			return
		}
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, chat)
}

type patchChatRequest struct {
	Read     *bool `json:"read,omitempty"`
	Archived *bool `json:"archived,omitempty"`
	Muted    *bool `json:"muted,omitempty"`
}

func (s *Server) handlePatchChat(w http.ResponseWriter, r *http.Request) {
	acct, chatter, ok := s.resolveChatAccount(w, r, r.URL.Query().Get("account_id"))
	if !ok {
		return
	}
	chatID := r.PathValue("id")
	var req patchChatRequest
	if err := decodeJSON(r, &req); err != nil {
		writeDecodeError(w, err)
		return
	}
	if req.Read == nil && req.Archived == nil && req.Muted == nil {
		writeError(w, http.StatusBadRequest, "empty_patch", "supply read, archived, and/or muted")
		return
	}
	if _, err := s.store.GetChat(acct.ID, chatID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "no such chat")
			return
		}
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}

	if req.Read != nil && *req.Read {
		msgs, _, err := s.store.ListChatMessages(acct.ID, chatID, "", 50)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal", err.Error())
			return
		}
		ids := make([]string, 0, len(msgs))
		for _, m := range msgs {
			ids = append(ids, m.ID)
		}
		if len(ids) > 0 {
			if err := chatter.MarkRead(r.Context(), acct.ID, chatID, ids); err != nil {
				writeProviderError(w, err)
				return
			}
		}
		if err := s.store.ClearUnread(acct.ID, chatID); err != nil {
			writeError(w, http.StatusInternalServerError, "internal", err.Error())
			return
		}
		if len(ids) > 0 {
			s.dispatcher.Emit(model.Event{Type: model.EventChatUpdated, AccountID: acct.ID, MessageIDs: ids, Status: "read"})
		}
	}
	if req.Archived != nil || req.Muted != nil {
		if err := s.store.SetChatFlags(acct.ID, chatID, req.Archived, req.Muted); err != nil {
			writeError(w, http.StatusInternalServerError, "internal", err.Error())
			return
		}
	}

	chat, err := s.store.GetChat(acct.ID, chatID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if req.Archived != nil || req.Muted != nil {
		s.dispatcher.Emit(model.Event{Type: model.EventChatUpdated, AccountID: acct.ID, Chat: &chat, Change: "flags"})
	}
	writeJSON(w, http.StatusOK, chat)
}

// ---- messages ----

func (s *Server) handleListChatMessages(w http.ResponseWriter, r *http.Request) {
	acct, _, ok := s.resolveChatAccount(w, r, r.URL.Query().Get("account_id"))
	if !ok {
		return
	}
	chatID := r.PathValue("id")
	limit, _ := paging(r)
	before := r.URL.Query().Get("before")
	msgs, next, err := s.store.ListChatMessages(acct.ID, chatID, before, limit)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "unknown before id")
			return
		}
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, struct {
		Items      []model.ChatMessage `json:"items"`
		NextBefore string              `json:"next_before,omitempty"`
	}{Items: msgs, NextBefore: next})
}

// sendChatText is the send path shared by handleSendChatMessage and
// handleStartChat's follow-up send: mint a temp row, hand the text to the
// provider, then either promote the row to the provider's id and status or,
// on failure, remove it entirely. A failed send must leave no trace — the
// caller never sees a "sending" message that will never resolve.
func (s *Server) sendChatText(ctx context.Context, acct model.Account, chatter provider.Chatter, chatID, text, quotedID string) (model.ChatMessage, int, any) {
	tmpID, err := accounts.NewID("tmp")
	if err != nil {
		return model.ChatMessage{}, http.StatusInternalServerError, apiErr("internal", err.Error())
	}
	self, err := s.selfAttendee(acct)
	if err != nil {
		return model.ChatMessage{}, http.StatusInternalServerError, apiErr("internal", err.Error())
	}
	row := model.ChatMessage{
		AccountID: acct.ID, ID: tmpID, ChatID: chatID, Sender: self, IsFromMe: true,
		Kind: "text", Text: text, QuotedMessageID: quotedID, SentAt: time.Now().UTC(), Status: "sending",
	}
	if _, err := s.store.UpsertChatMessage(row); err != nil {
		return model.ChatMessage{}, http.StatusInternalServerError, apiErr("internal", err.Error())
	}
	res, err := chatter.SendText(ctx, acct.ID, chatID, text, quotedID)
	if err != nil {
		_ = s.store.DeleteChatMessageRow(acct.ID, tmpID)
		// Same mapping the command routes use: a send against an account with
		// no live socket is 409 reconnect_required, not a bare 502.
		status, body := providerError(err)
		return model.ChatMessage{}, status, body
	}
	// Promote the tmp row to the provider's id. This can lose a race: the
	// chat runtime's own socket may deliver this same send back as an
	// inbound "echo" and insert it under res.MessageID before we get here,
	// so the rename's UPDATE collides on the (account_id, id) primary key.
	// When that happens the echo row already holds the right content, so the
	// tmp row is now pure debris — discard it and report the echo row as the
	// outcome instead of failing the request.
	if err := s.store.RenameChatMessage(acct.ID, tmpID, res.MessageID); err != nil {
		_ = s.store.DeleteChatMessageRow(acct.ID, tmpID)
	}
	if err := s.store.SetMessageStatus(acct.ID, []string{res.MessageID}, "sent"); err != nil {
		return model.ChatMessage{}, http.StatusInternalServerError, apiErr("internal", err.Error())
	}
	_ = s.store.BumpChat(acct.ID, chatID, row.SentAt, 0)
	full, err := s.store.GetChatMessage(acct.ID, res.MessageID)
	if err != nil {
		// Neither the renamed tmp row nor a pre-existing echo row exists —
		// genuinely unexpected (SetMessageStatus above touched zero rows too
		// in that case), so this is the one path that must surface as a 500
		// rather than silently returning an empty message.
		return model.ChatMessage{}, http.StatusInternalServerError, apiErr("internal", err.Error())
	}
	return full, http.StatusCreated, nil
}

func (s *Server) handleSendChatMessage(w http.ResponseWriter, r *http.Request) {
	acct, chatter, ok := s.resolveChatAccount(w, r, r.URL.Query().Get("account_id"))
	if !ok {
		return
	}
	chatID := r.PathValue("id")

	r, raw, err := readRawBody(r)
	if err != nil {
		writeDecodeError(w, err)
		return
	}
	var req struct {
		Text            string `json:"text"`
		QuotedMessageID string `json:"quoted_message_id,omitempty"`
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body", err.Error())
		return
	}
	if strings.TrimSpace(req.Text) == "" {
		writeError(w, http.StatusBadRequest, "missing_text", "text is required")
		return
	}

	dev, _ := developerFrom(r.Context())
	s.withIdempotency(w, r, dev.ID, acct.ID, func() (int, any) {
		if _, err := s.store.GetChat(acct.ID, chatID); err != nil {
			return http.StatusNotFound, apiErr("not_found", "no such chat")
		}
		full, status, apierr := s.sendChatText(r.Context(), acct, chatter, chatID, req.Text, req.QuotedMessageID)
		if apierr != nil {
			return status, apierr
		}
		c, err := s.store.GetChat(acct.ID, chatID)
		if err != nil {
			return http.StatusInternalServerError, apiErr("internal", err.Error())
		}
		s.dispatcher.Emit(model.Event{Type: model.EventChatSent, AccountID: acct.ID, Message: &full, Chat: &c})
		return status, full
	})
}

func (s *Server) handleGetChatMessage(w http.ResponseWriter, r *http.Request) {
	acct, _, ok := s.resolveChatAccount(w, r, r.URL.Query().Get("account_id"))
	if !ok {
		return
	}
	m, err := s.store.GetChatMessage(acct.ID, r.PathValue("mid"))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "no such message")
			return
		}
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if m.ChatID != r.PathValue("id") {
		writeError(w, http.StatusNotFound, "not_found", "no such message")
		return
	}
	writeJSON(w, http.StatusOK, m)
}

// messageAndChat fetches a message and enforces both that it exists and that
// it belongs to the chat named in the path — the shared prelude for the
// three routes that mutate one message.
func (s *Server) messageAndChat(w http.ResponseWriter, r *http.Request, acct model.Account) (model.ChatMessage, bool) {
	m, err := s.store.GetChatMessage(acct.ID, r.PathValue("mid"))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "no such message")
			return model.ChatMessage{}, false
		}
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return model.ChatMessage{}, false
	}
	if m.ChatID != r.PathValue("id") {
		writeError(w, http.StatusNotFound, "not_found", "no such message")
		return model.ChatMessage{}, false
	}
	return m, true
}

func (s *Server) handlePatchChatMessage(w http.ResponseWriter, r *http.Request) {
	acct, chatter, ok := s.resolveChatAccount(w, r, r.URL.Query().Get("account_id"))
	if !ok {
		return
	}
	var req struct {
		Text string `json:"text"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeDecodeError(w, err)
		return
	}
	if strings.TrimSpace(req.Text) == "" {
		writeError(w, http.StatusBadRequest, "missing_text", "text is required")
		return
	}
	m, ok := s.messageAndChat(w, r, acct)
	if !ok {
		return
	}
	if !m.IsFromMe {
		writeError(w, http.StatusForbidden, "not_own_message", "cannot edit another attendee's message")
		return
	}
	chatID, mid := r.PathValue("id"), r.PathValue("mid")
	if err := chatter.Edit(r.Context(), acct.ID, chatID, mid, req.Text); err != nil {
		writeProviderError(w, err)
		return
	}
	if err := s.store.EditChatMessage(acct.ID, mid, req.Text, time.Now().UTC()); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	full, err := s.store.GetChatMessage(acct.ID, mid)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	s.dispatcher.Emit(model.Event{Type: model.EventChatUpdated, AccountID: acct.ID, MessageIDs: []string{mid}, Change: "edited", Message: &full})
	writeJSON(w, http.StatusOK, full)
}

func (s *Server) handleDeleteChatMessage(w http.ResponseWriter, r *http.Request) {
	acct, chatter, ok := s.resolveChatAccount(w, r, r.URL.Query().Get("account_id"))
	if !ok {
		return
	}
	m, ok := s.messageAndChat(w, r, acct)
	if !ok {
		return
	}
	if !m.IsFromMe {
		writeError(w, http.StatusForbidden, "not_own_message", "cannot delete another attendee's message")
		return
	}
	chatID, mid := r.PathValue("id"), r.PathValue("mid")
	if err := chatter.Delete(r.Context(), acct.ID, chatID, mid); err != nil {
		writeProviderError(w, err)
		return
	}
	if err := s.store.RevokeChatMessage(acct.ID, mid); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	s.dispatcher.Emit(model.Event{Type: model.EventChatDeleted, AccountID: acct.ID, MessageIDs: []string{mid}})
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleReactToMessage(w http.ResponseWriter, r *http.Request) {
	acct, chatter, ok := s.resolveChatAccount(w, r, r.URL.Query().Get("account_id"))
	if !ok {
		return
	}
	// Emoji is a *string, not a string: the spec makes "" a meaningful,
	// documented request ("remove this reaction"), so it must stay
	// distinguishable from the field being left out of the body entirely,
	// which is far more likely a client bug (a dropped field, an empty
	// struct) than a deliberate removal.
	var req struct {
		Emoji *string `json:"emoji"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeDecodeError(w, err)
		return
	}
	if req.Emoji == nil {
		writeError(w, http.StatusBadRequest, "missing_emoji",
			`emoji is required (send "" to remove an existing reaction)`)
		return
	}
	emoji := *req.Emoji
	if _, ok := s.messageAndChat(w, r, acct); !ok {
		return
	}
	chatID, mid := r.PathValue("id"), r.PathValue("mid")
	self, err := s.selfAttendee(acct)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if err := chatter.React(r.Context(), acct.ID, chatID, mid, emoji); err != nil {
		writeProviderError(w, err)
		return
	}
	reaction := model.Reaction{AttendeeID: self.ID, Emoji: emoji, At: time.Now().UTC()}
	if err := s.store.ApplyReaction(acct.ID, mid, reaction); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "no such message")
			return
		}
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	s.dispatcher.Emit(model.Event{Type: model.EventChatReaction, AccountID: acct.ID, MessageIDs: []string{mid}, Reaction: &reaction})
	w.WriteHeader(http.StatusNoContent)
}

// ---- attendees ----

func (s *Server) handleListAttendees(w http.ResponseWriter, r *http.Request) {
	acct, _, ok := s.resolveChatAccount(w, r, r.URL.Query().Get("account_id"))
	if !ok {
		return
	}
	limit, offset := paging(r)
	atts, err := s.store.ListAttendees(acct.ID, r.URL.Query().Get("q"), limit, offset)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, listResponse[model.Attendee]{Items: atts, Limit: limit, Offset: offset})
}

func (s *Server) handleGetAttendee(w http.ResponseWriter, r *http.Request) {
	acct, _, ok := s.resolveChatAccount(w, r, r.URL.Query().Get("account_id"))
	if !ok {
		return
	}
	att, err := s.store.GetAttendee(acct.ID, r.PathValue("id"))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "no such attendee")
			return
		}
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, att)
}
