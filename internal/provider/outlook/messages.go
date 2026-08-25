package outlook

import (
	"context"
	"encoding/base64"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gauravrautela/unified-messaging/internal/model"
	"github.com/gauravrautela/unified-messaging/internal/provider"
)

// msgSelect keeps delta and list pages to the fields we normalize. Including
// `body` makes pages heavier but means one pass yields complete messages
// instead of N follow-up GETs during backfill.
const msgSelect = "id,conversationId,parentFolderId,subject,bodyPreview,body,from,sender," +
	"toRecipients,ccRecipients,bccRecipients,replyTo,receivedDateTime,sentDateTime," +
	"isRead,isDraft,hasAttachments,flag,internetMessageId"

// SyncMessages walks one folder's delta round to completion.
func (c *Client) SyncMessages(ctx context.Context, accountID string, scope provider.Scope,
	cursor string, since time.Time) (provider.Changes, error) {

	var res provider.Changes

	next := cursor
	if next == "" {
		next = fmt.Sprintf("/me/mailFolders/%s/messages/delta?$select=%s",
			url.PathEscape(scope.ID), msgSelect)
		// Bound the initial backfill. receivedDateTime ge/gt is the *only*
		// $filter delta accepts for messages, and it must be supplied on the
		// first call — Graph bakes it into every subsequent delta token.
		if !since.IsZero() {
			next += "&$filter=receivedDateTime+ge+" + since.UTC().Format("2006-01-02T15:04:05Z")
		}
	}

	for next != "" {
		var page messagesPage
		err := c.do(ctx, accountID, request{
			method:  http.MethodGet,
			url:     next,
			out:     &page,
			headers: map[string]string{"Prefer": "odata.maxpagesize=50"},
		})
		if err != nil {
			return res, err
		}

		for _, m := range page.Value {
			if m.Removed != nil {
				res.Removed = append(res.Removed, m.ID)
				continue
			}
			e := m.toModel(accountID)
			// Delta omits parentFolderId on some update events; the folder we
			// asked about is the right answer either way.
			if e.FolderID == "" {
				e.FolderID = scope.ID
			}
			res.Changed = append(res.Changed, e)
		}

		if page.DeltaLink != "" {
			res.Cursor = page.DeltaLink
			return res, nil
		}
		if page.NextLink == next {
			// Defensive: a non-advancing nextLink would spin forever.
			return res, fmt.Errorf("outlook: delta nextLink did not advance for folder %s", scope.ID)
		}
		next = page.NextLink
	}
	return res, nil
}

func (c *Client) GetMessage(ctx context.Context, accountID, messageID string) (model.Email, error) {
	var m graphMessage
	err := c.do(ctx, accountID, request{
		method: http.MethodGet,
		url:    "/me/messages/" + url.PathEscape(messageID) + "?$select=" + msgSelect,
		out:    &m,
	})
	if err != nil {
		return model.Email{}, err
	}
	return m.toModel(accountID), nil
}

func (c *Client) UpdateMessage(ctx context.Context, accountID, messageID string, upd provider.MessageUpdate) error {
	patch := map[string]any{}
	if upd.Read != nil {
		patch["isRead"] = *upd.Read
	}
	if upd.Flagged != nil {
		status := "notFlagged"
		if *upd.Flagged {
			status = "flagged"
		}
		patch["flag"] = map[string]any{"flagStatus": status}
	}
	if len(patch) == 0 {
		return nil
	}
	return c.do(ctx, accountID, request{
		method: http.MethodPatch,
		url:    "/me/messages/" + url.PathEscape(messageID),
		body:   patch,
	})
}

// ---- attachments ----

func (c *Client) ListAttachments(ctx context.Context, accountID, messageID string) ([]model.Attachment, error) {
	var page struct {
		Value []graphAttachment `json:"value"`
	}
	err := c.do(ctx, accountID, request{
		method: http.MethodGet,
		url: "/me/messages/" + url.PathEscape(messageID) +
			// contentId exists only on the fileAttachment subtype; selecting it on
			// the base attachment collection is a 400 on the real service.
			"/attachments?$select=id,name,contentType,size,isInline",
		out: &page,
	})
	if err != nil {
		return nil, err
	}
	out := make([]model.Attachment, 0, len(page.Value))
	for _, a := range page.Value {
		out = append(out, a.toModel())
	}
	return out, nil
}

// DownloadAttachment uses Graph's $value endpoint, avoiding a base64 blob
// dragged through JSON.
func (c *Client) DownloadAttachment(ctx context.Context, accountID, messageID, attachmentID string) ([]byte, error) {
	var raw []byte
	err := c.do(ctx, accountID, request{
		method: http.MethodGet,
		url: "/me/messages/" + url.PathEscape(messageID) +
			"/attachments/" + url.PathEscape(attachmentID) + "/$value",
		raw: &raw,
	})
	return raw, err
}

// ---- sending ----

type outgoing struct {
	Subject       string            `json:"subject,omitempty"`
	Body          *itemBody         `json:"body,omitempty"`
	ToRecipients  []recipient       `json:"toRecipients,omitempty"`
	CcRecipients  []recipient       `json:"ccRecipients,omitempty"`
	BccRecipients []recipient       `json:"bccRecipients,omitempty"`
	Attachments   []graphAttachment `json:"attachments,omitempty"`
}

func buildAttachments(in []model.SendAttachment) ([]graphAttachment, error) {
	if len(in) == 0 {
		return nil, nil
	}
	out := make([]graphAttachment, 0, len(in))
	for _, a := range in {
		if _, err := base64.StdEncoding.DecodeString(a.Content); err != nil {
			return nil, fmt.Errorf("attachment %q: content must be base64: %w", a.Name, err)
		}
		ct := a.MimeType
		if ct == "" {
			ct = "application/octet-stream"
		}
		out = append(out, graphAttachment{
			ODataType:    "#microsoft.graph.fileAttachment",
			Name:         a.Name,
			ContentType:  ct,
			ContentBytes: a.Content,
		})
	}
	return out, nil
}

func bodyFor(content, bodyType string) *itemBody {
	if strings.ToLower(bodyType) == "text" {
		return &itemBody{ContentType: "text", Content: content}
	}
	return &itemBody{ContentType: "html", Content: content}
}

// Send delivers a brand-new message. Graph's /sendMail returns no sent item, so
// there is no message ID to report back.
func (c *Client) Send(ctx context.Context, accountID string, req model.SendRequest) (provider.SendResult, error) {
	atts, err := buildAttachments(req.Attachments)
	if err != nil {
		return provider.SendResult{}, err
	}
	payload := map[string]any{
		"message": outgoing{
			Subject:       req.Subject,
			Body:          bodyFor(req.Body, req.BodyType),
			ToRecipients:  fromRecipients(req.To),
			CcRecipients:  fromRecipients(req.Cc),
			BccRecipients: fromRecipients(req.Bcc),
			Attachments:   atts,
		},
		"saveToSentItems": true,
	}
	err = c.do(ctx, accountID, request{
		method: http.MethodPost,
		url:    "/me/sendMail",
		body:   payload,
	})
	return provider.SendResult{}, err
}

// Reply replies in-thread.
//
// The three-step createReply → PATCH → send dance matters: Graph populates
// conversationId, In-Reply-To and References on the draft it creates. Composing
// a fresh message and setting those headers by hand does not thread reliably,
// and Graph rejects most attempts to set them directly.
func (c *Client) Reply(ctx context.Context, accountID, messageID string, req model.SendRequest) (provider.SendResult, error) {
	action := "createReply"
	if req.ReplyAll {
		action = "createReplyAll"
	}

	var draft graphMessage
	if err := c.do(ctx, accountID, request{
		method: http.MethodPost,
		url:    "/me/messages/" + url.PathEscape(messageID) + "/" + action,
		out:    &draft,
	}); err != nil {
		return provider.SendResult{}, err
	}
	err := c.fillAndSendDraft(ctx, accountID, draft, req)
	return provider.SendResult{MessageID: draft.ID}, err
}

func (c *Client) Forward(ctx context.Context, accountID, messageID string, req model.SendRequest) (provider.SendResult, error) {
	var draft graphMessage
	if err := c.do(ctx, accountID, request{
		method: http.MethodPost,
		url:    "/me/messages/" + url.PathEscape(messageID) + "/createForward",
		out:    &draft,
	}); err != nil {
		return provider.SendResult{}, err
	}
	err := c.fillAndSendDraft(ctx, accountID, draft, req)
	return provider.SendResult{MessageID: draft.ID}, err
}

// fillAndSendDraft writes our content into a Graph-generated reply or forward
// draft and sends it. Our text goes *above* the quoted original Graph already
// placed in the draft body, matching what every mail client does.
func (c *Client) fillAndSendDraft(ctx context.Context, accountID string, draft graphMessage, req model.SendRequest) error {
	quoted := ""
	quotedType := "html"
	if draft.Body != nil {
		quoted = draft.Body.Content
		if draft.Body.ContentType != "" {
			quotedType = strings.ToLower(draft.Body.ContentType)
		}
	}

	newBody := req.Body
	if strings.ToLower(req.BodyType) == "text" && quotedType == "html" {
		newBody = textToHTML(req.Body)
	}

	patch := map[string]any{
		"body": itemBody{ContentType: quotedType, Content: newBody + quoted},
	}
	// Recipients default to whatever createReply/createReplyAll chose; override
	// only when the caller was explicit. Forwards have no default recipient, so
	// they must supply one.
	if len(req.To) > 0 {
		patch["toRecipients"] = fromRecipients(req.To)
	}
	if len(req.Cc) > 0 {
		patch["ccRecipients"] = fromRecipients(req.Cc)
	}
	if len(req.Bcc) > 0 {
		patch["bccRecipients"] = fromRecipients(req.Bcc)
	}
	if req.Subject != "" {
		patch["subject"] = req.Subject
	}
	if atts, err := buildAttachments(req.Attachments); err != nil {
		return err
	} else if len(atts) > 0 {
		patch["attachments"] = atts
	}

	if err := c.do(ctx, accountID, request{
		method: http.MethodPatch,
		url:    "/me/messages/" + url.PathEscape(draft.ID),
		body:   patch,
	}); err != nil {
		return err
	}
	return c.do(ctx, accountID, request{
		method: http.MethodPost,
		url:    "/me/messages/" + url.PathEscape(draft.ID) + "/send",
	})
}

func (c *Client) CreateDraft(ctx context.Context, accountID string, req model.SendRequest) (model.Email, error) {
	atts, err := buildAttachments(req.Attachments)
	if err != nil {
		return model.Email{}, err
	}
	var created graphMessage
	err = c.do(ctx, accountID, request{
		method: http.MethodPost,
		url:    "/me/messages",
		body: outgoing{
			Subject:       req.Subject,
			Body:          bodyFor(req.Body, req.BodyType),
			ToRecipients:  fromRecipients(req.To),
			CcRecipients:  fromRecipients(req.Cc),
			BccRecipients: fromRecipients(req.Bcc),
			Attachments:   atts,
		},
		out: &created,
	})
	if err != nil {
		return model.Email{}, err
	}
	return created.toModel(accountID), nil
}

func (c *Client) SendDraft(ctx context.Context, accountID, draftID string) error {
	return c.do(ctx, accountID, request{
		method: http.MethodPost,
		url:    "/me/messages/" + url.PathEscape(draftID) + "/send",
	})
}

func textToHTML(s string) string {
	return "<div>" + strings.ReplaceAll(html.EscapeString(s), "\n", "<br>") + "</div>"
}
