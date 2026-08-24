// Package model holds the provider-neutral shapes our REST API speaks.
//
// The whole point of a Unipile-style service is that callers never see a
// Microsoft Graph payload. Everything Graph returns is mapped into these types
// at the edge (internal/graph), so a second provider could later be added
// without changing internal/api.
package model

import "time"

// Account states, mirroring the lifecycle a connected mailbox actually has.
const (
	AccountOK = "OK" // token valid, syncing
	// AccountCredentials means the refresh token was rejected. Only the end user
	// can fix this, by walking the connect flow again.
	AccountCredentials  = "CREDENTIALS"
	AccountDisconnected = "DISCONNECTED" // deleted by us
)

type Account struct {
	ID        string    `json:"id"`
	Provider  string    `json:"provider"` // always "OUTLOOK" in this POC
	Email     string    `json:"email"`
	Name      string    `json:"name"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	// LastSyncedAt is nil until the first backfill completes.
	LastSyncedAt *time.Time `json:"last_synced_at,omitempty"`
}

type Recipient struct {
	Name  string `json:"name,omitempty"`
	Email string `json:"email"`
}

type Folder struct {
	ID          string `json:"id"`
	AccountID   string `json:"account_id"`
	Name        string `json:"name"`
	ParentID    string `json:"parent_id,omitempty"`
	Role        string `json:"role,omitempty"` // inbox, sent, drafts, ... when well-known
	TotalCount  int    `json:"total_count"`
	UnreadCount int    `json:"unread_count"`
}

type Attachment struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	MimeType  string `json:"mime_type"`
	Size      int64  `json:"size"`
	IsInline  bool   `json:"is_inline"`
	ContentID string `json:"content_id,omitempty"`
}

type Email struct {
	ID        string `json:"id"`
	AccountID string `json:"account_id"`
	ThreadID  string `json:"thread_id"`
	FolderID  string `json:"folder_id"`

	Subject string      `json:"subject"`
	From    Recipient   `json:"from"`
	To      []Recipient `json:"to"`
	Cc      []Recipient `json:"cc,omitempty"`
	Bcc     []Recipient `json:"bcc,omitempty"`
	ReplyTo []Recipient `json:"reply_to,omitempty"`

	Date     time.Time `json:"date"`
	Snippet  string    `json:"snippet"`
	Body     string    `json:"body,omitempty"`
	BodyType string    `json:"body_type,omitempty"` // "html" or "text"

	Read           bool `json:"read"`
	Flagged        bool `json:"flagged"`
	Draft          bool `json:"draft"`
	HasAttachments bool `json:"has_attachments"`

	// InternetMessageID is the RFC 5322 Message-ID. It is the only identifier
	// that survives leaving the mailbox, so it is what a cross-provider system
	// would key threading on.
	InternetMessageID string       `json:"internet_message_id,omitempty"`
	Attachments       []Attachment `json:"attachments,omitempty"`
}

type Thread struct {
	ID        string    `json:"id"`
	AccountID string    `json:"account_id"`
	Subject   string    `json:"subject"`
	Count     int       `json:"count"`
	LastDate  time.Time `json:"last_date"`
	Unread    int       `json:"unread"`
}

// SendRequest is the normalized outbound-mail shape.
type SendRequest struct {
	To      []Recipient `json:"to"`
	Cc      []Recipient `json:"cc,omitempty"`
	Bcc     []Recipient `json:"bcc,omitempty"`
	Subject string      `json:"subject"`
	Body    string      `json:"body"`
	// BodyType defaults to "html".
	BodyType string `json:"body_type,omitempty"`
	// ReplyTo, when set, is the ID of the message being replied to. We use Graph's
	// createReply so the provider populates conversationId, In-Reply-To and
	// References itself rather than us hand-rolling headers.
	ReplyTo string `json:"reply_to_email_id,omitempty"`
	// ReplyAll only applies alongside ReplyTo.
	ReplyAll    bool             `json:"reply_all,omitempty"`
	Attachments []SendAttachment `json:"attachments,omitempty"`
}

type SendAttachment struct {
	Name     string `json:"name"`
	MimeType string `json:"mime_type"`
	// Content is base64-encoded. Graph caps inline attachments around 3 MB;
	// larger files need an upload session, which is out of scope for the POC.
	Content string `json:"content"`
}

// Webhook is a caller-registered endpoint we deliver normalized events to.
type Webhook struct {
	ID        string    `json:"id"`
	URL       string    `json:"url"`
	Secret    string    `json:"secret,omitempty"`
	Events    []string  `json:"events"`
	CreatedAt time.Time `json:"created_at"`
}

// Event names we emit.
const (
	EventMailReceived = "mail_received"
	EventMailSent     = "mail_sent"
	EventMailUpdated  = "mail_updated"
	EventMailDeleted  = "mail_deleted"
	EventAccountError = "account_status"
)

type Event struct {
	Type      string    `json:"type"`
	AccountID string    `json:"account_id"`
	Timestamp time.Time `json:"timestamp"`
	Email     *Email    `json:"email,omitempty"`
	EmailID   string    `json:"email_id,omitempty"`
	Account   *Account  `json:"account,omitempty"`
}
