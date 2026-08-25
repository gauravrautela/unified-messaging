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
// validates ownership the same way, then insists the account's provider is a
// chat one. resolveID itself already tolerates a chat account fine (Mailbox()
// is nil, mailboxFor reports no error for that), so the only extra step here
// is capability-checking Chat() rather than the mail contract.
func (s *Server) resolveChatAccount(w http.ResponseWriter, r *http.Request, id string) (model.Account, provider.Chatter, bool) {
	acct, _, ok := s.resolveID(w, r, id)
	if !ok {
		return model.Account{}, nil, false
	}
	p, err := s.registry.Get(acct.Provider)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "unknown_provider", err.Error())
		return model.Account{}, nil, false
	}
	chatter := p.Chat()
	if chatter == nil {
		writeError(w, http.StatusBadRequest, "unsupported_for_kind", "this account does not support chat")
		return model.Account{}, nil, false
	}
	return acct, chatter, true
}

// selfAttendee resolves the account's own attendee row, falling back to a
// bare stand-in keyed by the account's identifier when the roster sync has
// not populated one yet (e.g. straight after linking, before the first
// Chats() pull completes).
func (s *Server) selfAttendee(acct model.Account) (model.Attendee, error) {
	self, err := s.store.SelfAttendee(acct.ID)
	if errors.Is(err, store.ErrNotFound) {
		return model.Attendee{ID: acct.Identifier, IsSelf: true}, nil
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
// rather than a second parameter so the call site can stay the shape the
// send-path design settled on: s.withIdempotency(w, r, dev.ID, do).

type rawBodyCtxKey struct{}

func readRawBody(r *http.Request) (*http.Request, []byte, error) {
	if r.Body == nil {
		return r, nil, nil
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 32<<20))
	if err != nil {
		return r, nil, err
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

// withIdempotency runs do exactly once per (developer, Idempotency-Key)
// pair. No header at all means "not idempotent": do runs every time. A
// header that was seen before with the same request body hash replays the
// stored response without calling do again; a different hash under the same
// key is a client bug and gets 409 idempotency_conflict. Keys are purged
// lazily (anything older than 24h) on every successful write so the table
// never grows unbounded without a separate sweep job.
func (s *Server) withIdempotency(w http.ResponseWriter, r *http.Request, devID string, do func() (int, any)) {
	key := r.Header.Get("Idempotency-Key")
	if key == "" {
		status, body := do()
		writeJSON(w, status, body)
		return
	}

	sum := sha256.Sum256(rawBodyFrom(r.Context()))
	hash := hex.EncodeToString(sum[:])

	if stored, err := s.store.GetIdempotency(devID, key); err == nil {
		var rec idempotencyRecord
		if json.Unmarshal(stored, &rec) == nil {
			if rec.Hash != hash {
				writeError(w, http.StatusConflict, "idempotency_conflict",
					"this Idempotency-Key was already used with a different request body")
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(rec.Status)
			_, _ = w.Write(rec.Body)
			return
		}
	} else if !errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}

	status, body := do()
	bodyJSON, err := json.Marshal(body)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if status >= 200 && status < 300 {
		if recJSON, err := json.Marshal(idempotencyRecord{Hash: hash, Status: status, Body: bodyJSON}); err == nil {
			s.store.PurgeIdempotency(time.Now().Add(-24 * time.Hour))
			_ = s.store.PutIdempotency(devID, key, recJSON)
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(bodyJSON)
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
		writeError(w, http.StatusBadRequest, "invalid_body", err.Error())
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
	s.withIdempotency(w, r, dev.ID, func() (int, any) {
		phone := req.Phone
		if req.AttendeeID != "" {
			att, err := s.store.GetAttendee(acct.ID, req.AttendeeID)
			if err != nil {
				if errors.Is(err, store.ErrNotFound) {
					return http.StatusNotFound, apiErr("not_found", "no such attendee")
				}
				return http.StatusInternalServerError, apiErr("internal", err.Error())
			}
			phone = att.Phone
		}

		chatID, err := chatter.StartDirect(r.Context(), acct.ID, phone)
		if err != nil {
			return http.StatusBadGateway, apiErr("provider_error", err.Error())
		}
		if err := s.store.UpsertChat(model.Chat{ID: chatID, AccountID: acct.ID, Kind: "direct"}); err != nil {
			return http.StatusInternalServerError, apiErr("internal", err.Error())
		}
		attID := req.AttendeeID
		if attID == "" {
			attID = chatID
		}
		if err := s.store.UpsertAttendee(model.Attendee{ID: attID, Phone: phone}, acct.ID); err != nil {
			return http.StatusInternalServerError, apiErr("internal", err.Error())
		}

		full, status, apierr := s.sendChatText(r.Context(), acct, chatter, chatID, req.Text, "")
		if apierr != nil {
			return status, apierr
		}
		c, _ := s.store.GetChat(acct.ID, chatID)
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
		writeError(w, http.StatusBadRequest, "invalid_body", err.Error())
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
		return model.ChatMessage{}, http.StatusBadGateway, apiErr("provider_error", err.Error())
	}
	_ = s.store.RenameChatMessage(acct.ID, tmpID, res.MessageID)
	_ = s.store.SetMessageStatus(acct.ID, []string{res.MessageID}, "sent")
	_ = s.store.BumpChat(acct.ID, chatID, row.SentAt, 0)
	full, err := s.store.GetChatMessage(acct.ID, res.MessageID)
	if err != nil {
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
		writeError(w, http.StatusBadRequest, "invalid_body", err.Error())
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
	s.withIdempotency(w, r, dev.ID, func() (int, any) {
		if _, err := s.store.GetChat(acct.ID, chatID); err != nil {
			return http.StatusNotFound, apiErr("not_found", "no such chat")
		}
		full, status, apierr := s.sendChatText(r.Context(), acct, chatter, chatID, req.Text, req.QuotedMessageID)
		if apierr != nil {
			return status, apierr
		}
		c, _ := s.store.GetChat(acct.ID, chatID)
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
		writeError(w, http.StatusBadRequest, "invalid_body", err.Error())
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
	var req struct {
		Emoji string `json:"emoji"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body", err.Error())
		return
	}
	if _, ok := s.messageAndChat(w, r, acct); !ok {
		return
	}
	chatID, mid := r.PathValue("id"), r.PathValue("mid")
	self, err := s.selfAttendee(acct)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if err := chatter.React(r.Context(), acct.ID, chatID, mid, req.Emoji); err != nil {
		writeProviderError(w, err)
		return
	}
	reaction := model.Reaction{AttendeeID: self.ID, Emoji: req.Emoji, At: time.Now().UTC()}
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
