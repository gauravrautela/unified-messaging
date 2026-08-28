package whatsapp

import (
	"errors"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"

	"github.com/gauravrautela/unified-messaging/internal/logx"
	"github.com/gauravrautela/unified-messaging/internal/provider"
)

// testSession builds a link session driven by a QR channel the test owns, with
// no client behind it: whatsmeow's Disconnect is nil-safe, so every code path
// through resolve can run without a socket. cancelled reports whether the
// session cancelled the QR context, which is what stops the library's emitter.
func testSession(t *testing.T) (*linkSession, chan whatsmeow.QRChannelItem, func() bool) {
	t.Helper()
	log, _ := logx.Capture()
	p := &Provider{log: log, conns: map[string]*conn{}}
	cancelled := false
	s := newLinkSession(p, log, nil, nil, func() { cancelled = true })
	qr := make(chan whatsmeow.QRChannelItem, 8)
	go s.pump(qr)
	return s, qr, func() bool { return cancelled }
}

// waitPump fails the test if the pump goroutine outlives the session.
func waitPump(t *testing.T, s *linkSession) {
	t.Helper()
	select {
	case <-s.pumpDone:
	case <-time.After(2 * time.Second):
		t.Fatal("pump goroutine did not exit after the session resolved")
	}
}

// Closing a session must leave nothing behind. whatsmeow's QR emitter returns
// without closing its channel when the client disconnects, so a pump that only
// ranged over that channel would pin the client and device forever.
func TestLinkSessionCloseStopsEverything(t *testing.T) {
	s, qr, cancelled := testSession(t)
	qr <- whatsmeow.QRChannelItem{Event: whatsmeow.QRChannelEventCode, Code: "2@abc", Timeout: 20 * time.Second}

	code, ok := <-s.Codes()
	if !ok || code.Code != "2@abc" || code.ExpiresAt.IsZero() {
		t.Fatalf("code = %+v ok=%v", code, ok)
	}

	s.Close()
	waitPump(t, s) // the QR channel is deliberately never closed here

	if !cancelled() {
		t.Fatal("Close must cancel the QR context so whatsmeow stops emitting")
	}
	res := <-s.Result()
	if !errors.Is(res.Err, provider.ErrLinkCancelled) {
		t.Fatalf("result = %+v, want ErrLinkCancelled", res)
	}
	if _, open := <-s.Codes(); open {
		t.Fatal("Codes() must be closed once the session ends")
	}
	// Closing twice is a no-op, not a panic or a second result.
	s.Close()
	select {
	case extra := <-s.Result():
		t.Fatalf("second result delivered: %+v", extra)
	default:
	}
}

// The QR channel's terminal items each map to one outcome.
func TestLinkSessionTerminalItems(t *testing.T) {
	boom := errors.New("pair failed")
	cases := []struct {
		name string
		item whatsmeow.QRChannelItem
		want error
	}{
		{"timeout", whatsmeow.QRChannelTimeout, provider.ErrLinkTimeout},
		{"error", whatsmeow.QRChannelItem{Event: whatsmeow.QRChannelEventError, Error: boom}, boom},
		{"client outdated", whatsmeow.QRChannelClientOutdated, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, qr, cancelled := testSession(t)
			qr <- tc.item
			res := <-s.Result()
			waitPump(t, s)
			if !cancelled() {
				t.Fatal("resolving must cancel the QR context")
			}
			switch {
			case tc.want != nil && !errors.Is(res.Err, tc.want):
				t.Fatalf("result = %+v, want %v", res, tc.want)
			case tc.want == nil && res.Err == nil:
				t.Fatalf("result = %+v, want an error", res)
			}
		})
	}
}

// A QR channel that closes without a terminal item still resolves the caller.
func TestLinkSessionChannelCloseResolves(t *testing.T) {
	s, qr, _ := testSession(t)
	close(qr)
	res := <-s.Result()
	waitPump(t, s)
	if !errors.Is(res.Err, provider.ErrLinkTimeout) {
		t.Fatalf("result = %+v, want ErrLinkTimeout", res)
	}
}
