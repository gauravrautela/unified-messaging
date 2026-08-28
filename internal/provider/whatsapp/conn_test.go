package whatsapp

import (
	"errors"
	"sync"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"

	"github.com/gauravrautela/unified-messaging/internal/logx"
	"github.com/gauravrautela/unified-messaging/internal/model"
)

// recordingSink captures what the adapter publishes, so the event handler can
// be driven with no socket in sight.
type recordingSink struct {
	mu           sync.Mutex
	messages     []model.ChatMessage
	receipts     []string
	reactions    []model.Reaction
	deleted      []string
	edited       []string
	disconnected []disconnect
}

type disconnect struct {
	reason    string
	loggedOut bool
}

func (s *recordingSink) Message(accountID string, m model.ChatMessage, chat model.Chat, sender model.Attendee) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.messages = append(s.messages, m)
}

func (s *recordingSink) Receipt(accountID, chatID string, messageIDs []string, status string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.receipts = append(s.receipts, status)
}

func (s *recordingSink) Reaction(accountID, chatID, messageID string, r model.Reaction) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reactions = append(s.reactions, r)
}

func (s *recordingSink) Edited(accountID, chatID, messageID, text string, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.edited = append(s.edited, messageID)
}

func (s *recordingSink) Deleted(accountID, chatID, messageID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deleted = append(s.deleted, messageID)
}

func (s *recordingSink) Disconnected(accountID, reason string, loggedOut bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.disconnected = append(s.disconnected, disconnect{reason, loggedOut})
}

// testConn builds a connection with no client: handle() only translates and
// publishes, so nothing it touches needs a socket.
func testConn(t *testing.T) (*conn, *recordingSink, *logx.Records) {
	t.Helper()
	log, recs := logx.Capture()
	p := &Provider{log: log.With("component", "whatsapp"), conns: map[string]*conn{}}
	sink := &recordingSink{}
	return &conn{p: p, accountID: "acc_1", sink: sink}, sink, recs
}

// Every way a WhatsApp connection can die has to reach the sink; a socket the
// runtime believes is alive is worse than one it knows is gone.
func TestHandleDisconnectEvents(t *testing.T) {
	cases := []struct {
		name      string
		evt       any
		reason    string
		loggedOut bool
	}{
		{"logged out", &events.LoggedOut{Reason: events.ConnectFailureLoggedOut},
			"logged out: " + events.ConnectFailureLoggedOut.String(), true},
		{"temporary ban", &events.TemporaryBan{Code: events.TempBanSentToTooManyPeople},
			"temporary ban: " + events.TempBanSentToTooManyPeople.String(), true},
		{"stream replaced", &events.StreamReplaced{}, "stream replaced", false},
		{"connect failure", &events.ConnectFailure{Reason: events.ConnectFailureServiceUnavailable},
			"connect failure: " + events.ConnectFailureServiceUnavailable.String(), false},
		{"client outdated", &events.ClientOutdated{}, "client outdated", false},
		{"cat refresh error", &events.CATRefreshError{Error: errors.New("boom")}, "cat refresh error: boom", false},
		{"disconnected", &events.Disconnected{}, "disconnected", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, sink, _ := testConn(t)
			c.handle(tc.evt)
			if len(sink.disconnected) != 1 {
				t.Fatalf("%d disconnect events, want 1", len(sink.disconnected))
			}
			got := sink.disconnected[0]
			if got.reason != tc.reason || got.loggedOut != tc.loggedOut {
				t.Fatalf("disconnect = %+v, want reason %q loggedOut %v", got, tc.reason, tc.loggedOut)
			}
		})
	}
}

// Protocol machinery must not surface as a chat message.
func TestHandleDropsProtocolNoise(t *testing.T) {
	c, sink, _ := testConn(t)
	chat := types.NewJID("919888000000", types.DefaultUserServer)
	c.handle(evt(chat, "P", &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{
		Type: waE2E.ProtocolMessage_APP_STATE_SYNC_KEY_SHARE.Enum(),
		Key:  &waCommon.MessageKey{ID: proto.String("A")}}}))
	if len(sink.messages) != 0 || len(sink.deleted) != 0 || len(sink.edited) != 0 || len(sink.reactions) != 0 {
		t.Fatalf("protocol message published: %+v", sink)
	}
	// A revoke, by contrast, is real news.
	c.handle(evt(chat, "R", &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{
		Type: waE2E.ProtocolMessage_REVOKE.Enum(),
		Key:  &waCommon.MessageKey{ID: proto.String("A")}}}))
	if len(sink.deleted) != 1 {
		t.Fatalf("revoke not published: %+v", sink.deleted)
	}
}

// Debug logs correlate conversations without spelling out phone numbers.
func TestHandleLogsDigestedChatID(t *testing.T) {
	c, _, recs := testConn(t)
	chat := types.NewJID("919888000000", types.DefaultUserServer)
	c.handle(evt(chat, "M", &waE2E.Message{Conversation: proto.String("hello there")}))
	c.handle(&events.Receipt{
		MessageSource: types.MessageSource{Chat: chat},
		MessageIDs:    []types.MessageID{"M"},
		Type:          types.ReceiptTypeRead,
	})
	if recs.Contains("919888000000") {
		t.Fatalf("logs leak the phone number: %v", recs.All())
	}
	if !recs.Contains(logx.Digest(chat.String())) {
		t.Fatalf("logs are missing the chat digest: %v", recs.All())
	}
	if recs.Contains("hello there") {
		t.Fatalf("logs leak message text: %v", recs.All())
	}
	if !recs.Contains("text_bytes=11") {
		t.Fatalf("logs are missing the text size: %v", recs.All())
	}
}
