package outlook

import (
	"html"
	"regexp"
	"strings"
	"time"

	"github.com/gauravrautela/unified-messaging/internal/model"
)

// ---- Graph wire types (only the fields we actually consume) ----

type emailAddress struct {
	Name    string `json:"name,omitempty"`
	Address string `json:"address,omitempty"`
}

type recipient struct {
	EmailAddress emailAddress `json:"emailAddress"`
}

type itemBody struct {
	ContentType string `json:"contentType,omitempty"` // "html" | "text"
	Content     string `json:"content,omitempty"`
}

type followupFlag struct {
	FlagStatus string `json:"flagStatus,omitempty"` // notFlagged | flagged | complete
}

type graphMessage struct {
	ID                string        `json:"id"`
	ConversationID    string        `json:"conversationId"`
	ParentFolderID    string        `json:"parentFolderId"`
	Subject           string        `json:"subject"`
	BodyPreview       string        `json:"bodyPreview"`
	Body              *itemBody     `json:"body"`
	From              *recipient    `json:"from"`
	Sender            *recipient    `json:"sender"`
	ToRecipients      []recipient   `json:"toRecipients"`
	CcRecipients      []recipient   `json:"ccRecipients"`
	BccRecipients     []recipient   `json:"bccRecipients"`
	ReplyTo           []recipient   `json:"replyTo"`
	ReceivedDateTime  string        `json:"receivedDateTime"`
	SentDateTime      string        `json:"sentDateTime"`
	IsRead            *bool         `json:"isRead"`
	IsDraft           *bool         `json:"isDraft"`
	HasAttachments    *bool         `json:"hasAttachments"`
	Flag              *followupFlag `json:"flag"`
	InternetMessageID string        `json:"internetMessageId"`

	// Present only in delta responses, for removed items.
	Removed *struct {
		Reason string `json:"reason"`
	} `json:"@removed"`
}

type graphFolder struct {
	ID               string `json:"id"`
	DisplayName      string `json:"displayName"`
	ParentFolderID   string `json:"parentFolderId"`
	TotalItemCount   int    `json:"totalItemCount"`
	UnreadItemCount  int    `json:"unreadItemCount"`
	ChildFolderCount int    `json:"childFolderCount"`

	Removed *struct {
		Reason string `json:"reason"`
	} `json:"@removed"`
}

type graphAttachment struct {
	ODataType    string `json:"@odata.type"`
	ID           string `json:"id"`
	Name         string `json:"name"`
	ContentType  string `json:"contentType"`
	Size         int64  `json:"size"`
	IsInline     bool   `json:"isInline"`
	ContentID    string `json:"contentId"`
	ContentBytes string `json:"contentBytes,omitempty"`
}

// messagesPage is the shape of both plain collections and delta rounds.
type messagesPage struct {
	Value     []graphMessage `json:"value"`
	NextLink  string         `json:"@odata.nextLink"`
	DeltaLink string         `json:"@odata.deltaLink"`
}

type foldersPage struct {
	Value     []graphFolder `json:"value"`
	NextLink  string        `json:"@odata.nextLink"`
	DeltaLink string        `json:"@odata.deltaLink"`
}

// ---- mapping into the normalized model ----

func toRecipients(rs []recipient) []model.Recipient {
	out := make([]model.Recipient, 0, len(rs))
	for _, r := range rs {
		if r.EmailAddress.Address == "" && r.EmailAddress.Name == "" {
			continue
		}
		out = append(out, model.Recipient{Name: r.EmailAddress.Name, Email: r.EmailAddress.Address})
	}
	return out
}

func fromRecipients(rs []model.Recipient) []recipient {
	out := make([]recipient, 0, len(rs))
	for _, r := range rs {
		out = append(out, recipient{EmailAddress: emailAddress{Name: r.Name, Address: r.Email}})
	}
	return out
}

func (m graphMessage) toModel(accountID string) model.Email {
	e := model.Email{
		ID:                m.ID,
		AccountID:         accountID,
		ThreadID:          m.ConversationID,
		FolderID:          m.ParentFolderID,
		Subject:           m.Subject,
		To:                toRecipients(m.ToRecipients),
		Cc:                toRecipients(m.CcRecipients),
		Bcc:               toRecipients(m.BccRecipients),
		ReplyTo:           toRecipients(m.ReplyTo),
		Snippet:           m.BodyPreview,
		InternetMessageID: m.InternetMessageID,
	}

	// Drafts and sent items have no meaningful `from` until they leave, so fall
	// back to `sender` rather than showing an empty author.
	src := m.From
	if src == nil || src.EmailAddress.Address == "" {
		src = m.Sender
	}
	if src != nil {
		e.From = model.Recipient{Name: src.EmailAddress.Name, Email: src.EmailAddress.Address}
	}

	// receivedDateTime is absent on drafts; sentDateTime is the sane fallback.
	e.Date = parseTime(m.ReceivedDateTime)
	if e.Date.IsZero() {
		e.Date = parseTime(m.SentDateTime)
	}

	if m.Body != nil {
		e.Body = m.Body.Content
		e.BodyType = strings.ToLower(m.Body.ContentType)
	}
	if e.Snippet == "" && e.Body != "" {
		e.Snippet = snippetFrom(e.Body, e.BodyType)
	}
	if m.IsRead != nil {
		e.Read = *m.IsRead
	}
	if m.IsDraft != nil {
		e.Draft = *m.IsDraft
	}
	if m.HasAttachments != nil {
		e.HasAttachments = *m.HasAttachments
	}
	if m.Flag != nil {
		e.Flagged = m.Flag.FlagStatus == "flagged"
	}
	return e
}

func (f graphFolder) toModel(accountID string) model.Folder {
	return model.Folder{
		ID:          f.ID,
		AccountID:   accountID,
		Name:        f.DisplayName,
		ParentID:    f.ParentFolderID,
		TotalCount:  f.TotalItemCount,
		UnreadCount: f.UnreadItemCount,
	}
}

func (a graphAttachment) toModel() model.Attachment {
	return model.Attachment{
		ID:        a.ID,
		Name:      a.Name,
		MimeType:  a.ContentType,
		Size:      a.Size,
		IsInline:  a.IsInline,
		ContentID: a.ContentID,
	}
}

func parseTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t.UTC()
}

// Go's RE2 has no backreferences, so script/style blocks are stripped by their
// own pattern before the generic tag pass.
var (
	scriptStyleRE = regexp.MustCompile(`(?is)<(script|style)\b[^>]*>.*?</(script|style)\s*>`)
	tagRE         = regexp.MustCompile(`(?s)<[^>]*>`)
)

func snippetFrom(body, bodyType string) string {
	text := body
	if bodyType == "html" {
		stripped := scriptStyleRE.ReplaceAllString(body, " ")
		text = html.UnescapeString(tagRE.ReplaceAllString(stripped, " "))
	}
	text = strings.Join(strings.Fields(text), " ")
	if len(text) > 255 {
		text = text[:255]
	}
	return text
}
