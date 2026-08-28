package model

import "time"

// Account kinds. A mail account is read through a Mailbox; a chat account is a
// live linked device read through a Chatter.
const (
	AccountKindMail = "mail"
	AccountKindChat = "chat"
)

// Connection is the live state of a chat account's socket, reported by the
// chat runtime. Nil for mail accounts.
type Connection struct {
	State      string    `json:"state"` // connecting | connected | backoff | stopped | error
	Since      time.Time `json:"since"`
	Reconnects int       `json:"reconnects"`
	LastError  string    `json:"last_error,omitempty"`
}

type Chat struct {
	ID            string     `json:"id"`
	AccountID     string     `json:"account_id"`
	Kind          string     `json:"kind"` // direct | group | status | channel
	Name          string     `json:"name"`
	UnreadCount   int        `json:"unread_count"`
	LastMessageAt *time.Time `json:"last_message_at,omitempty"`
	Archived      bool       `json:"archived"`
	Muted         bool       `json:"muted"`
	Members       []Attendee `json:"members,omitempty"`
}

// Attendee is a person in an account's chats. ID is the stable provider id
// (phone JID when known, else a privacy id); Phone is E.164 when resolvable.
type Attendee struct {
	ID     string `json:"id"`
	Phone  string `json:"phone,omitempty"`
	Name   string `json:"name"`
	IsSelf bool   `json:"is_self"`
}

type ChatMember struct {
	ChatID     string `json:"chat_id"`
	AttendeeID string `json:"attendee_id"`
	Role       string `json:"role,omitempty"` // admin | ""
}

type Reaction struct {
	AttendeeID string    `json:"attendee_id"`
	Emoji      string    `json:"emoji"`
	At         time.Time `json:"at"`
}

type ChatMessage struct {
	ID              string     `json:"id"`
	AccountID       string     `json:"account_id"`
	ChatID          string     `json:"chat_id"`
	Sender          Attendee   `json:"sender"`
	IsFromMe        bool       `json:"is_from_me"`
	Kind            string     `json:"kind"` // text | unsupported
	Text            string     `json:"text"`
	QuotedMessageID string     `json:"quoted_message_id,omitempty"`
	SentAt          time.Time  `json:"sent_at"`
	EditedAt        *time.Time `json:"edited_at,omitempty"`
	Deleted         bool       `json:"deleted"`
	Status          string     `json:"status,omitempty"` // own messages: sending | sent | delivered | read
	Reactions       []Reaction `json:"reactions"`
}

// Chat event names.
const (
	EventChatReceived = "chat_received"
	EventChatSent     = "chat_sent"
	EventChatUpdated  = "chat_updated"
	EventChatReaction = "chat_reaction"
	EventChatDeleted  = "chat_deleted"
)
