package notify

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gauravrautela/unified-messaging/internal/model"
)

// Sender delivers one event to one hook. payload is the JSON event as the
// dispatcher encoded it; only the raw webhook uses it, the chat targets
// render their own text with Format. A nil error means the target accepted
// the notification; any error is retried on the dispatcher's schedule and
// must already be scrubbed of credentials.
type Sender interface {
	Send(ctx context.Context, h model.Webhook, ev model.Event, payload []byte, attempt int) error
}

// ErrTelegramRejected marks a Telegram "ok": false answer; the wrapped
// message is Telegram's description. Callers map it to a 400.
var ErrTelegramRejected = errors.New("telegram rejected the target")

// StatusError is a non-2xx answer from a target, carrying the HTTP status as a
// number so the delivery log can record it without anyone parsing the message.
// Msg is the whole error text and is already scrubbed; Error returns it
// verbatim, so wrapping a status in this type never changes what a caller
// reads back from last_error.
type StatusError struct {
	Code int
	Msg  string
}

func (e *StatusError) Error() string { return e.Msg }

// StatusOf reports the HTTP status an error carries, or 0 when it carries
// none — a transport failure, or a target that never answered.
func StatusOf(err error) int {
	var se *StatusError
	if errors.As(err, &se) {
		return se.Code
	}
	return 0
}

const (
	discordMax  = 2000
	telegramMax = 4096
	telegramAPI = "https://api.telegram.org"
)

// Registry hands out the Sender for a hook kind. One per process.
type Registry struct {
	client       *http.Client
	telegramBase string
}

func NewRegistry(client *http.Client) *Registry {
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	return &Registry{client: client, telegramBase: telegramAPI}
}

// SetTelegramBase points Telegram calls at a test server.
func (r *Registry) SetTelegramBase(base string) { r.telegramBase = strings.TrimRight(base, "/") }

func (r *Registry) For(kind string) (Sender, bool) {
	switch kind {
	case model.WebhookKindWebhook, "":
		return webhookSender{client: r.client}, true
	case model.WebhookKindDiscord:
		return discordSender{client: r.client}, true
	case model.WebhookKindTelegram:
		return telegramSender{client: r.client, base: r.telegramBase}, true
	}
	return nil, false
}

// ValidateTelegram checks the token/chat pair with getChat. A Telegram
// rejection returns ErrTelegramRejected (wrapped); a transport problem
// returns a plain error.
//
// The timeout is the only thing holding the create handler open (the server
// sets no WriteTimeout), so it is deliberately tighter than the delivery
// client's: a getChat that has not answered in 5 s is better reported as
// 502 provider_error than left hanging on the caller.
func (r *Registry) ValidateTelegram(ctx context.Context, botToken, chatID string) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, err := telegramCall(ctx, r.client, r.telegramBase, botToken, "getChat", map[string]any{"chat_id": chatID})
	return err
}

// ---- webhook: the JSON event, signed ----

type webhookSender struct{ client *http.Client }

func (s webhookSender) Send(ctx context.Context, h model.Webhook, _ model.Event, payload []byte, attempt int) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.URL, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Outlook-Event", eventType(payload))
	req.Header.Set("X-Outlook-Delivery", fmt.Sprintf("%d", attempt))
	if h.Secret != "" {
		mac := hmac.New(sha256.New, []byte(h.Secret))
		mac.Write(payload)
		req.Header.Set("X-Outlook-Signature", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode >= 300 {
		return &StatusError{Code: resp.StatusCode, Msg: fmt.Sprintf("status %d", resp.StatusCode)}
	}
	return nil
}

// eventType reads "type" back out of the encoded payload so the header and
// body can never disagree.
func eventType(payload []byte) string {
	var v struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal(payload, &v)
	return v.Type
}

// ---- discord: incoming webhook, Markdown ----

type discordSender struct{ client *http.Client }

func (s discordSender) Send(ctx context.Context, h model.Webhook, ev model.Event, _ []byte, _ int) error {
	body, _ := json.Marshal(map[string]any{
		// discordMax-1 leaves room for the ellipsis truncate appends when it
		// cuts: the cap is inclusive and one rune over is a 400, which the
		// dispatcher would retry seven times before dead-lettering.
		"content":          truncate(Format(ev, Markdown), discordMax-1),
		"allowed_mentions": map[string]any{"parse": []string{}},
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.URL, bytes.NewReader(body))
	if err != nil {
		return ScrubErr(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		return ScrubErr(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
		return &StatusError{Code: resp.StatusCode,
			Msg: fmt.Sprintf("discord: status %d %s", resp.StatusCode, Scrub(strings.TrimSpace(string(snippet))))}
	}
	return nil
}

// ---- telegram: Bot API sendMessage, HTML ----

type telegramSender struct {
	client *http.Client
	base   string
}

func (s telegramSender) Send(ctx context.Context, h model.Webhook, ev model.Event, _ []byte, _ int) error {
	if h.Telegram == nil || h.Telegram.BotToken == "" || h.Telegram.ChatID == "" {
		return errors.New("telegram: config unreadable")
	}
	_, err := telegramCall(ctx, s.client, s.base, h.Telegram.BotToken, "sendMessage", map[string]any{
		"chat_id": h.Telegram.ChatID,
		// telegramMax-1: see the discordMax-1 note above.
		"text":                     truncate(Format(ev, HTML), telegramMax-1),
		"parse_mode":               "HTML",
		"disable_web_page_preview": true,
	})
	return err
}

type telegramResponse struct {
	OK          bool            `json:"ok"`
	Description string          `json:"description"`
	Result      json.RawMessage `json:"result"`
}

// telegramCall POSTs one Bot API method. Errors never contain the token.
func telegramCall(ctx context.Context, client *http.Client, base, token, method string, params map[string]any) (json.RawMessage, error) {
	body, _ := json.Marshal(params)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/bot"+token+"/"+method, bytes.NewReader(body))
	if err != nil {
		return nil, ScrubErr(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, ScrubErr(err)
	}
	defer resp.Body.Close()
	var tr telegramResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&tr); err != nil {
		return nil, fmt.Errorf("telegram: %s: status %d, unreadable body", method, resp.StatusCode)
	}
	if !tr.OK {
		return nil, fmt.Errorf("%w: %s", ErrTelegramRejected, Scrub(tr.Description))
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("telegram: %s: status %d", method, resp.StatusCode)
	}
	return tr.Result, nil
}
