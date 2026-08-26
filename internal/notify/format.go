package notify

import (
	"fmt"
	"html"
	"strings"
	"unicode/utf8"

	"github.com/gauravrautela/unified-messaging/internal/model"
)

// Flavour is the markup a target understands.
type Flavour int

const (
	Markdown Flavour = iota // Discord
	HTML                    // Telegram (parse_mode=HTML)
)

const (
	mailSnippet = 200
	chatSnippet = 300
)

// Format renders one event as a short notification. Every value that came
// from a user (subject, names, message text) is escaped for the flavour;
// phone numbers are masked. Unknown event types still render, so a new
// event never blocks delivery.
func Format(ev model.Event, f Flavour) string {
	b := &builder{f: f}
	acct := ev.AccountID
	if ev.Account != nil && ev.Account.Email != "" {
		acct = ev.Account.Email
	}
	switch ev.Type {
	case model.EventMailReceived:
		b.head("📧", "New mail", acct)
		if ev.Email != nil {
			b.line("From: " + recipient(ev.Email.From))
			b.bold(ev.Email.Subject)
			body := ev.Email.BodyPlain
			if body == "" {
				body = ev.Email.Snippet
			}
			b.snippet(body, mailSnippet)
		}
	case model.EventMailSent:
		b.head("📤", "Mail sent", acct)
		if ev.Email != nil {
			b.line("To: " + recipients(ev.Email.To))
			b.bold(ev.Email.Subject)
		}
	case model.EventMailUpdated:
		b.head("✏️", "Mail updated", acct)
		if ev.Email != nil {
			b.bold(ev.Email.Subject)
			b.line(fmt.Sprintf("read: %v · flagged: %v", ev.Email.Read, ev.Email.Flagged))
		}
	case model.EventMailDeleted:
		b.head("🗑", "Mail deleted", acct)
		b.line("id " + ev.EmailID)
	case model.EventChatReceived, model.EventChatSent:
		b.head("💬", "WhatsApp", chatName(ev))
		if ev.Message != nil {
			b.boldInline(attendee(ev.Message.Sender), ": ")
			b.snippetInline(ev.Message.Text, chatSnippet)
		}
	case model.EventChatUpdated:
		switch ev.Change {
		case "receipt":
			b.head("📬", "Message "+ev.Status, chatName(ev))
		default:
			b.head("✏️", "Message edited", chatName(ev))
			if ev.Message != nil {
				b.snippet(ev.Message.Text, chatSnippet)
			}
		}
	case model.EventChatReaction:
		b.head("👍", "Reaction", chatName(ev))
		who := ""
		if ev.Message != nil {
			who = attendee(ev.Message.Sender)
		}
		if ev.Reaction == nil || ev.Reaction.Emoji == "" {
			b.line("reaction removed" + by(who))
		} else {
			b.line(ev.Reaction.Emoji + by(who))
		}
	case model.EventChatDeleted:
		b.head("🗑", "Message deleted", chatName(ev))
	case model.EventAccountError:
		status := ""
		if ev.Account != nil {
			status = ev.Account.Status
		}
		b.head("⚠️", "Account needs attention", acct)
		b.line("→ " + status + " — relink from the dashboard")
	default:
		// Type and AccountID are internal identifiers, not user content:
		// write them raw rather than through esc, which would mangle
		// underscores in event names like "chat_updated".
		b.sb.WriteString(ev.Type + " · " + ev.AccountID + "\n")
	}
	return strings.TrimRight(b.sb.String(), "\n")
}

func by(who string) string {
	if who == "" {
		return ""
	}
	return " by " + who
}

func chatName(ev model.Event) string {
	if ev.Chat != nil && ev.Chat.Name != "" {
		return ev.Chat.Name
	}
	if ev.Message != nil {
		return attendee(ev.Message.Sender)
	}
	return ev.AccountID
}

func attendee(a model.Attendee) string {
	if a.Name != "" {
		return a.Name
	}
	if a.Phone != "" {
		return MaskPhone(a.Phone)
	}
	return a.ID
}

func recipient(r model.Recipient) string {
	if r.Name != "" {
		return r.Name + " <" + r.Email + ">"
	}
	return r.Email
}

func recipients(rs []model.Recipient) string {
	parts := make([]string, 0, len(rs))
	for _, r := range rs {
		parts = append(parts, recipient(r))
	}
	return strings.Join(parts, ", ")
}

// builder writes lines in one flavour; every user value passes through esc.
type builder struct {
	f  Flavour
	sb strings.Builder
}

func (b *builder) esc(s string) string {
	switch b.f {
	case HTML:
		return html.EscapeString(s)
	default:
		return escapeMarkdown(s)
	}
}

func (b *builder) strong(s string) string {
	if b.f == HTML {
		return "<b>" + b.esc(s) + "</b>"
	}
	return "**" + b.esc(s) + "**"
}

func (b *builder) head(icon, title, where string) {
	b.sb.WriteString(icon + " " + b.strong(title) + " · " + b.esc(where) + "\n")
}
func (b *builder) line(s string) { b.sb.WriteString(b.esc(s) + "\n") }

// bold renders a secondary emphasis line, e.g. a mail subject. In HTML it
// uses <i> rather than <b>: <b> is reserved for the headline produced by
// head(), so a subject never collides with it in the rendered notification.
func (b *builder) bold(s string) {
	if b.f == HTML {
		b.sb.WriteString("<i>" + b.esc(s) + "</i>\n")
		return
	}
	b.sb.WriteString(b.strong(s) + "\n")
}
func (b *builder) boldInline(s, sep string) { b.sb.WriteString(b.strong(s) + sep) }
func (b *builder) snippet(s string, n int)  { b.sb.WriteString(b.esc(truncate(s, n)) + "\n") }
func (b *builder) snippetInline(s string, n int) {
	b.sb.WriteString(b.esc(truncate(s, n)) + "\n")
}

// truncate cuts at n runes, appending an ellipsis when it cut.
func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n]) + "…"
}

// ">" is deliberately not escaped: it is common in plain-text addresses like
// "Bob <b> <bob@x.com>" and Discord does not render a leading "> " from
// mid-line text as a blockquote anyway.
var mdReplacer = strings.NewReplacer(`\`, `\\`, "*", `\*`, "_", `\_`, "~", `\~`, "`", "\\`", "|", `\|`, "#", `\#`)

func escapeMarkdown(s string) string { return mdReplacer.Replace(s) }
