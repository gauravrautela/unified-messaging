package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/gauravrautela/unified-messaging/internal/logx"
	"github.com/gauravrautela/unified-messaging/internal/model"
	"github.com/gauravrautela/unified-messaging/internal/provider"
	"github.com/gauravrautela/unified-messaging/internal/store"
)

// resolve validates the ?account_id= every mail route requires and returns the
// mailbox implementation that owns it.
func (s *Server) resolve(w http.ResponseWriter, r *http.Request) (model.Account, provider.Mailbox, bool) {
	return s.resolveID(w, r, r.URL.Query().Get("account_id"))
}

// accountID prefers the query string's account_id — the convention every
// other {id}-in-path mail route uses — and falls back to the request body's,
// which the documented reply/forward/send payloads also carry. Checking the
// query first means a caller cannot bypass ownership scoping by putting a
// different account_id in the body than the one being probed via the URL.
func accountID(r *http.Request, fromBody string) string {
	if q := r.URL.Query().Get("account_id"); q != "" {
		return q
	}
	return fromBody
}

// resolveAccount validates ?account_id (or the given id) against the calling
// developer and returns the owning provider, with no capability check yet.
// It is the ownership-only core shared by resolveID (which adds the mailbox
// check) and resolveChatAccount (which adds the chatter check) — capability
// checks must happen after ownership so a cross-tenant probe always 404s
// before it can learn anything about what kind of account it hit.
func (s *Server) resolveAccount(w http.ResponseWriter, r *http.Request, id string) (model.Account, provider.Provider, bool) {
	log := logx.From(r.Context())
	if id == "" {
		writeError(w, http.StatusBadRequest, "missing_account_id", "account_id is required")
		return model.Account{}, nil, false
	}
	dev, _ := developerFrom(r.Context())
	acct, err := s.store.GetAccount(dev.ID, id)
	if errors.Is(err, store.ErrNotFound) {
		log.Debug("ownership check", "account_id", id, "result", "not owned or unknown")
		writeError(w, http.StatusNotFound, "account_not_found", "no such account: "+id)
		return model.Account{}, nil, false
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return model.Account{}, nil, false
	}
	log.Debug("ownership check", "account_id", id, "result", "ok", "provider", acct.Provider, "status", acct.Status)
	p, err := s.registry.Get(acct.Provider)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "unknown_provider", err.Error())
		return model.Account{}, nil, false
	}
	return acct, p, true
}

// resolveID is resolveAccount plus the mail capability check: a chat account
// has no Mailbox(), so without this a mail handler would dereference a nil
// interface. Every mail route goes through this (via resolve/accountID),
// which is what makes "chats on a mail account" and "mail on a chat account"
// both 400 unsupported_for_kind rather than a panic or a wrong-shaped 200.
func (s *Server) resolveID(w http.ResponseWriter, r *http.Request, id string) (model.Account, provider.Mailbox, bool) {
	acct, p, ok := s.resolveAccount(w, r, id)
	if !ok {
		return model.Account{}, nil, false
	}
	mailbox := p.Mailbox()
	if mailbox == nil {
		writeError(w, http.StatusBadRequest, "unsupported_for_kind",
			"this account is a chat account; use /api/v1/chats")
		return model.Account{}, nil, false
	}
	return acct, mailbox, true
}

type listResponse[T any] struct {
	Items  []T `json:"items"`
	Limit  int `json:"limit"`
	Offset int `json:"offset"`
}

func (s *Server) handleListFolders(w http.ResponseWriter, r *http.Request) {
	acct, _, ok := s.resolve(w, r)
	if !ok {
		return
	}
	folders, err := s.store.ListFolders(acct.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, listResponse[model.Folder]{Items: folders})
}

func (s *Server) handleListThreads(w http.ResponseWriter, r *http.Request) {
	acct, _, ok := s.resolve(w, r)
	if !ok {
		return
	}
	limit, offset := paging(r)
	threads, err := s.store.ListThreads(acct.ID, limit, offset)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, listResponse[model.Thread]{Items: threads, Limit: limit, Offset: offset})
}

func (s *Server) handleListEmails(w http.ResponseWriter, r *http.Request) {
	acct, _, ok := s.resolve(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	limit, offset := paging(r)

	query := store.EmailQuery{
		AccountID: acct.ID,
		FolderID:  q.Get("folder_id"),
		ThreadID:  q.Get("thread_id"),
		Search:    q.Get("q"),
		Limit:     limit,
		Offset:    offset,
	}

	// folder_role lets callers say "inbox" without knowing a provider's opaque
	// folder IDs.
	if role := q.Get("folder_role"); role != "" && query.FolderID == "" {
		folders, err := s.store.ListFolders(acct.ID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal", err.Error())
			return
		}
		for _, f := range folders {
			if f.Role == strings.ToLower(role) {
				query.FolderID = f.ID
				break
			}
		}
		if query.FolderID == "" {
			writeError(w, http.StatusBadRequest, "unknown_folder_role",
				"no folder with role "+role+" on this account")
			return
		}
	}
	if v := q.Get("unread"); v != "" {
		unread := v == "true" || v == "1"
		query.Unread = &unread
	}

	emails, err := s.store.ListEmails(query)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	// Bodies make list responses enormous; callers fetch one message to get it.
	for i := range emails {
		emails[i].Body = ""
	}
	writeJSON(w, http.StatusOK, listResponse[model.Email]{Items: emails, Limit: limit, Offset: offset})
}

// handleGetEmail serves from the local mirror, falling back to the provider for
// a message the sync engine has not reached yet (or one outside the backfill
// window), and caches what it finds.
func (s *Server) handleGetEmail(w http.ResponseWriter, r *http.Request) {
	acct, mailbox, ok := s.resolve(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")

	email, err := s.store.GetEmail(acct.ID, id)
	if err == nil {
		s.complete(r.Context(), mailbox, &email)
		writeJSON(w, http.StatusOK, email)
		return
	}
	if !errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}

	email, perr := mailbox.GetMessage(r.Context(), acct.ID, id)
	if perr != nil {
		writeProviderError(w, perr)
		return
	}
	if err := s.store.UpsertEmail(email); err != nil {
		s.log.Warn("caching fetched email", "err", err)
	}
	s.complete(r.Context(), mailbox, &email)
	writeJSON(w, http.StatusOK, email)
}

// complete fills in what a single-message response should carry beyond the
// stored row: the folder's well-known role, and attachment metadata (fetched
// once from the provider, then cached on the row). Failures degrade to the
// bare fields rather than failing the read.
func (s *Server) complete(ctx context.Context, mailbox provider.Mailbox, e *model.Email) {
	if folders, err := s.store.ListFolders(e.AccountID); err == nil {
		for _, f := range folders {
			if f.ID == e.FolderID {
				e.Role = f.Role
				break
			}
		}
	}
	if e.HasAttachments && len(e.Attachments) == 0 {
		atts, err := mailbox.ListAttachments(ctx, e.AccountID, e.ID)
		if err != nil {
			s.log.Warn("listing attachments", "account_id", e.AccountID, "email_id", e.ID, "err", err)
			return
		}
		e.Attachments = atts
		if err := s.store.UpsertEmail(*e); err != nil {
			s.log.Warn("caching attachments", "err", err)
		}
	}
}

type patchEmailRequest struct {
	Read    *bool `json:"read,omitempty"`
	Flagged *bool `json:"flagged,omitempty"`
}

func (s *Server) handlePatchEmail(w http.ResponseWriter, r *http.Request) {
	acct, mailbox, ok := s.resolve(w, r)
	if !ok {
		return
	}
	var req patchEmailRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body", err.Error())
		return
	}
	if req.Read == nil && req.Flagged == nil {
		writeError(w, http.StatusBadRequest, "empty_patch", "supply read and/or flagged")
		return
	}

	id := r.PathValue("id")
	upd := provider.MessageUpdate{Read: req.Read, Flagged: req.Flagged}
	if err := mailbox.UpdateMessage(r.Context(), acct.ID, id, upd); err != nil {
		writeProviderError(w, err)
		return
	}
	// Write through locally so the change is visible before the next sync round.
	if email, err := s.store.GetEmail(acct.ID, id); err == nil {
		if req.Read != nil {
			email.Read = *req.Read
		}
		if req.Flagged != nil {
			email.Flagged = *req.Flagged
		}
		if err := s.store.UpsertEmail(email); err != nil {
			s.log.Warn("local write-through failed", "err", err)
		}
		writeJSON(w, http.StatusOK, email)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// sendPayload embeds SendRequest so the wire format stays flat.
type sendPayload struct {
	AccountID string `json:"account_id"`
	model.SendRequest
}

func (s *Server) handleSendEmail(w http.ResponseWriter, r *http.Request) {
	var p sendPayload
	if err := decodeJSON(r, &p); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body", err.Error())
		return
	}
	acct, mailbox, ok := s.resolveID(w, r, p.AccountID)
	if !ok {
		return
	}
	if p.ReplyTo == "" && len(p.To) == 0 {
		writeError(w, http.StatusBadRequest, "missing_recipients", "to is required")
		return
	}

	// A send carrying reply_to_email_id is routed through the provider's reply
	// machinery, so threading headers are generated upstream rather than guessed.
	if p.ReplyTo != "" {
		res, err := mailbox.Reply(r.Context(), acct.ID, p.ReplyTo, p.SendRequest)
		if err != nil {
			writeProviderError(w, err)
			return
		}
		s.syncer.Wake(acct.ID)
		writeSendResult(w, res)
		return
	}

	res, err := mailbox.Send(r.Context(), acct.ID, p.SendRequest)
	if err != nil {
		writeProviderError(w, err)
		return
	}
	s.syncer.Wake(acct.ID)
	writeSendResult(w, res)
}

func (s *Server) handleReply(w http.ResponseWriter, r *http.Request) {
	var p sendPayload
	if err := decodeJSON(r, &p); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body", err.Error())
		return
	}
	acct, mailbox, ok := s.resolveID(w, r, accountID(r, p.AccountID))
	if !ok {
		return
	}
	res, err := mailbox.Reply(r.Context(), acct.ID, r.PathValue("id"), p.SendRequest)
	if err != nil {
		writeProviderError(w, err)
		return
	}
	s.syncer.Wake(acct.ID)
	writeSendResult(w, res)
}

func (s *Server) handleForward(w http.ResponseWriter, r *http.Request) {
	var p sendPayload
	if err := decodeJSON(r, &p); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body", err.Error())
		return
	}
	acct, mailbox, ok := s.resolveID(w, r, accountID(r, p.AccountID))
	if !ok {
		return
	}
	if len(p.To) == 0 {
		writeError(w, http.StatusBadRequest, "missing_recipients", "to is required when forwarding")
		return
	}
	res, err := mailbox.Forward(r.Context(), acct.ID, r.PathValue("id"), p.SendRequest)
	if err != nil {
		writeProviderError(w, err)
		return
	}
	s.syncer.Wake(acct.ID)
	writeSendResult(w, res)
}

func (s *Server) handleCreateDraft(w http.ResponseWriter, r *http.Request) {
	var p sendPayload
	if err := decodeJSON(r, &p); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body", err.Error())
		return
	}
	acct, mailbox, ok := s.resolveID(w, r, p.AccountID)
	if !ok {
		return
	}
	draft, err := mailbox.CreateDraft(r.Context(), acct.ID, p.SendRequest)
	if err != nil {
		writeProviderError(w, err)
		return
	}
	if err := s.store.UpsertEmail(draft); err != nil {
		s.log.Warn("caching draft", "err", err)
	}
	writeJSON(w, http.StatusCreated, draft)
}

func (s *Server) handleSendDraft(w http.ResponseWriter, r *http.Request) {
	acct, mailbox, ok := s.resolve(w, r)
	if !ok {
		return
	}
	if err := mailbox.SendDraft(r.Context(), acct.ID, r.PathValue("id")); err != nil {
		writeProviderError(w, err)
		return
	}
	s.syncer.Wake(acct.ID)
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "sent"})
}

func (s *Server) handleListAttachments(w http.ResponseWriter, r *http.Request) {
	acct, mailbox, ok := s.resolve(w, r)
	if !ok {
		return
	}
	atts, err := mailbox.ListAttachments(r.Context(), acct.ID, r.PathValue("id"))
	if err != nil {
		writeProviderError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, listResponse[model.Attachment]{Items: atts})
}

// handleDownloadAttachment streams bytes straight from the provider. Blobs are
// never cached locally: they dominate mailbox size and are rarely re-read.
func (s *Server) handleDownloadAttachment(w http.ResponseWriter, r *http.Request) {
	acct, mailbox, ok := s.resolve(w, r)
	if !ok {
		return
	}
	messageID, attachmentID := r.PathValue("id"), r.PathValue("aid")

	data, err := mailbox.DownloadAttachment(r.Context(), acct.ID, messageID, attachmentID)
	if err != nil {
		writeProviderError(w, err)
		return
	}

	contentType, filename := "application/octet-stream", "attachment"
	if atts, err := mailbox.ListAttachments(r.Context(), acct.ID, messageID); err == nil {
		for _, a := range atts {
			if a.ID == attachmentID {
				if a.MimeType != "" {
					contentType = a.MimeType
				}
				if a.Name != "" {
					filename = a.Name
				}
				break
			}
		}
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Disposition", "attachment; filename="+strconv.Quote(filename))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

// writeSendResult reports a send. MessageID is absent for providers that do not
// return one — Microsoft's /sendMail, for instance — in which case the sent item
// surfaces on the next sync.
func writeSendResult(w http.ResponseWriter, res provider.SendResult) {
	out := map[string]string{"status": "sent"}
	if res.MessageID != "" {
		out["message_id"] = res.MessageID
	}
	writeJSON(w, http.StatusAccepted, out)
}

func paging(r *http.Request) (limit, offset int) {
	limit, offset = 50, 0
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 200 {
			limit = n
		}
	}
	if v := r.URL.Query().Get("offset"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			offset = n
		}
	}
	return limit, offset
}
