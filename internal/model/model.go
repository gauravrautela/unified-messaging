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
	ID string `json:"id"`
	// DeveloperID is the owner. Never serialised: a caller only ever sees
	// their own accounts, so it carries no information for them.
	DeveloperID string    `json:"-"`
	Provider    string    `json:"provider"` // "OUTLOOK" or "WHATSAPP"
	Email       string    `json:"email"`
	Kind        string    `json:"kind"`
	Identifier  string    `json:"identifier"`
	Name        string    `json:"name"`
	Status      string    `json:"status"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
	// LastSyncedAt is nil until the first backfill completes.
	LastSyncedAt *time.Time `json:"last_synced_at,omitempty"`
	// Connection is the live socket state for a chat account. Nil for mail.
	Connection *Connection `json:"connection,omitempty"`
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
	// BodyPlain is the body as text, with markup stripped when BodyType is html.
	BodyPlain string `json:"body_plain,omitempty"`
	// Role is the well-known role of the folder the message is in (inbox,
	// sentitems, ...), when the folder has one. Set on events, not stored.
	Role string `json:"role,omitempty"`

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

// Webhook kinds. A "webhook" receives the JSON event; "discord" and
// "telegram" receive a formatted notification (see internal/notify).
const (
	WebhookKindWebhook  = "webhook"
	WebhookKindDiscord  = "discord"
	WebhookKindTelegram = "telegram"
)

// KnownWebhookKind reports whether k is one of the three delivery kinds.
func KnownWebhookKind(k string) bool {
	return k == WebhookKindWebhook || k == WebhookKindDiscord || k == WebhookKindTelegram
}

// TelegramTarget is where a kind=telegram hook posts. The bot token is the
// developer's own credential: sealed at rest, never serialised.
type TelegramTarget struct {
	ChatID   string `json:"chat_id"`
	BotToken string `json:"-"`
}

// Webhook is a caller-registered endpoint we deliver normalized events to.
type Webhook struct {
	ID          string `json:"id"`
	DeveloperID string `json:"-"`
	// Name is a caller-chosen label echoed in every delivery, so one endpoint
	// fed by several hooks can tell them apart.
	Name string `json:"name,omitempty"`
	// AccountID scopes the hook to one connected account. Empty means global:
	// the hook receives events from every account.
	AccountID string `json:"account_id,omitempty"`
	// Kind selects the transport and payload shape; see WebhookKind*.
	Kind string `json:"kind"`
	// URL is the developer endpoint (webhook) or the Discord incoming-webhook
	// URL (discord); unused for telegram.
	URL       string          `json:"url,omitempty"`
	Secret    string          `json:"secret,omitempty"`
	Telegram  *TelegramTarget `json:"telegram,omitempty"`
	Events    []string        `json:"events"`
	CreatedAt time.Time       `json:"created_at"`
}

// Event names we emit.
const (
	EventMailReceived = "mail_received"
	EventMailSent     = "mail_sent"
	EventMailUpdated  = "mail_updated"
	EventMailDeleted  = "mail_deleted"
	EventAccountError = "account_status"
)

// KnownEvent reports whether name is one we emit (or the "*" wildcard).
func KnownEvent(name string) bool {
	switch name {
	case "*", EventMailReceived, EventMailSent, EventMailUpdated, EventMailDeleted, EventAccountError,
		EventChatReceived, EventChatSent, EventChatUpdated, EventChatReaction, EventChatDeleted:
		return true
	}
	return false
}

// WebhookRef identifies, inside a delivery, the hook it was sent through.
type WebhookRef struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
}

type Event struct {
	Type      string    `json:"type"`
	AccountID string    `json:"account_id"`
	Timestamp time.Time `json:"timestamp"`
	// Webhook is filled in per delivery by the dispatcher.
	Webhook *WebhookRef `json:"webhook,omitempty"`
	Email   *Email      `json:"email,omitempty"`
	EmailID string      `json:"email_id,omitempty"`
	Account *Account    `json:"account,omitempty"`

	// Chat fields, set only on chat_* events.
	Message    *ChatMessage `json:"message,omitempty"`
	Chat       *Chat        `json:"chat,omitempty"`
	MessageIDs []string     `json:"message_ids,omitempty"`
	Status     string       `json:"status,omitempty"`
	Change     string       `json:"change,omitempty"`
	Reaction   *Reaction    `json:"reaction,omitempty"`
}

// Developer is a tenant: the integrator who signs in, holds API keys, and
// owns connected accounts.
type Developer struct {
	ID        string    `json:"id"`
	Email     string    `json:"email"`
	Name      string    `json:"name,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// APIKey is the listable view of a key. The full key is returned exactly
// once, at creation, and never stored.
type APIKey struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Prefix     string     `json:"prefix"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	RevokedAt  *time.Time `json:"revoked_at,omitempty"`
}
