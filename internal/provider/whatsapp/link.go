package whatsapp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"

	"github.com/gauravrautela/unified-messaging/internal/logx"
	"github.com/gauravrautela/unified-messaging/internal/provider"
)

// linkCleanupTimeout bounds the database work done while tearing a failed or
// cancelled pairing down: the caller's context is usually already gone by then.
const linkCleanupTimeout = 5 * time.Second

// StartLink begins a pairing attempt: a brand-new device, a client bound to
// it, and a QR channel that rotates codes until the phone scans one.
//
// The device is only a candidate. It is written to the database by whatsmeow
// when pairing succeeds, and deleted again here if the attempt fails, so a
// user who walks away never leaves a half-linked device row behind.
func (p *Provider) StartLink(ctx context.Context) (provider.LinkSession, error) {
	log := logx.From(ctx).With("component", "whatsapp")

	device := p.container.NewDevice()
	client := whatsmeow.NewClient(device, waLog.Noop)

	s := &linkSession{
		p:      p,
		log:    log,
		client: client,
		device: device,
		codes:  make(chan provider.LinkCode, 8),
		result: make(chan provider.LinkResult, 1),
	}

	// Pairing details only reach us as an event; the QR channel's own success
	// item carries no JID. Registered before Connect so nothing is missed, and
	// deliberately before GetQRChannel: whatsmeow dispatches events to handlers
	// synchronously in registration order, and the QR channel's handler closes
	// the code channel on PairSuccess. Registering first is what guarantees we
	// have already recorded the identity by the time the pump sees that close.
	client.AddEventHandler(func(evt any) {
		ps, ok := evt.(*events.PairSuccess)
		if !ok {
			return
		}
		// whatsmeow persists the device (Store.ID = jid, then Store.Save) inside
		// handlePair before it dispatches PairSuccess, so by the time we get here
		// GetDevice(jid) will find the row and the runtime can connect through it.
		jid := ps.ID
		if client.Store.ID != nil {
			jid = *client.Store.ID
		}
		s.resolve(provider.LinkResult{
			Identity:  provider.Identity{Identifier: "+" + jid.User, Name: ps.BusinessName},
			DeviceJID: jid.String(),
		})
	})

	// GetQRChannel must be attached before Connect, otherwise the pairing
	// events fire with nobody listening.
	qr, err := client.GetQRChannel(ctx)
	if err != nil {
		return nil, fmt.Errorf("whatsapp: qr channel: %w", err)
	}

	if err := client.Connect(); err != nil {
		return nil, fmt.Errorf("whatsapp: connect for pairing: %w", err)
	}

	go s.pump(qr)
	log.Info("whatsapp link session started")
	return s, nil
}

// linkSession is one pairing attempt. Codes stream until the session ends;
// Result resolves exactly once, whichever way it ends.
type linkSession struct {
	p      *Provider
	log    *slog.Logger
	client *whatsmeow.Client
	device *store.Device

	codes  chan provider.LinkCode
	result chan provider.LinkResult

	mu     sync.Mutex
	closed bool // guards sends on codes; set under mu together with closing it
	once   sync.Once
}

func (s *linkSession) Codes() <-chan provider.LinkCode    { return s.codes }
func (s *linkSession) Result() <-chan provider.LinkResult { return s.result }

// Close cancels an in-flight session. Harmless after the session resolved.
func (s *linkSession) Close() {
	s.resolve(provider.LinkResult{Err: provider.ErrLinkCancelled})
}

// pump forwards QR codes to the caller and turns the channel's terminal item
// into a result. whatsmeow closes the channel right after that item.
func (s *linkSession) pump(qr <-chan whatsmeow.QRChannelItem) {
	for item := range qr {
		switch item.Event {
		case whatsmeow.QRChannelEventCode:
			s.emit(provider.LinkCode{Code: item.Code, ExpiresAt: time.Now().Add(item.Timeout)})
		case whatsmeow.QRChannelSuccess.Event:
			// The PairSuccess handler already resolved with the identity; this
			// item carries no JID, so there is nothing to add.
		case whatsmeow.QRChannelTimeout.Event:
			s.resolve(provider.LinkResult{Err: provider.ErrLinkTimeout})
		case whatsmeow.QRChannelEventError:
			err := item.Error
			if err == nil {
				err = errors.New("whatsapp: pairing failed")
			}
			s.resolve(provider.LinkResult{Err: err})
		case whatsmeow.QRChannelEventPasskeyRequest, whatsmeow.QRChannelEventPasskeyResponse:
			// Passkey pairing is not offered by this service; ignore.
		default:
			// err-client-outdated, err-scanned-without-multidevice, err-unexpected-state.
			s.resolve(provider.LinkResult{Err: fmt.Errorf("whatsapp: pairing failed: %s", item.Event)})
		}
	}
	// The channel closed without a terminal item we recognised: treat it as an
	// expired window rather than leaving the caller waiting forever.
	s.resolve(provider.LinkResult{Err: provider.ErrLinkTimeout})
}

// emit hands a code to the caller, dropping it if the caller is not keeping
// up. Blocking here would stall whatsmeow's QR emitter, which gives up and
// tears the session down when its own buffer fills.
func (s *linkSession) emit(c provider.LinkCode) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	select {
	case s.codes <- c:
	default:
		s.log.Warn("whatsapp link code dropped; reader is behind")
	}
}

// resolve delivers the outcome exactly once and tears the pairing client down.
//
// On success the client is disconnected but the device row is kept: the chat
// runtime opens the real connection itself, through GetDevice(jid), so that
// the connection it manages is the only live socket for the account. On any
// other outcome the candidate device is deleted so no orphan row survives.
func (s *linkSession) resolve(r provider.LinkResult) {
	s.once.Do(func() {
		s.mu.Lock()
		s.closed = true
		close(s.codes)
		s.mu.Unlock()

		// Tear the pairing socket down before handing the result over, so the
		// runtime's own connection is never racing this one for the device.
		// Disconnect only closes the websocket; it joins no goroutines, so it is
		// safe to call from inside an event handler.
		s.client.Disconnect()
		if r.Err != nil {
			s.discard()
			s.log.Info("whatsapp link session ended", "error", r.Err.Error())
		} else {
			s.log.Info("whatsapp link session paired")
		}

		// Buffered and never closed: the contract is one value, and a closed
		// channel would hand a second, empty result to anyone reading twice.
		s.result <- r
	})
}

// discard removes a candidate device that never finished pairing. A device
// that was never saved has no JID and therefore no row to delete.
func (s *linkSession) discard() {
	if s.device == nil || s.device.ID == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), linkCleanupTimeout)
	defer cancel()
	if err := s.p.container.DeleteDevice(ctx, s.device); err != nil {
		s.log.Warn("whatsapp: deleting unpaired device", "error", err.Error())
	}
}
