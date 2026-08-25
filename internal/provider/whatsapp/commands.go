package whatsapp

import (
	"context"
	"errors"

	"github.com/gauravrautela/unified-messaging/internal/provider"
)

// Outbound commands land in task 5. The stubs here exist so the adapter
// satisfies provider.Chatter today: they still enforce the one rule that will
// not change — a command needs a live connection for the account — and report
// plainly that the rest is not wired yet.

// errNotImplemented is the placeholder every command returns while connected.
var errNotImplemented = errors.New("whatsapp: not implemented until task 5")

// requireConn is the guard every command shares.
func (p *Provider) requireConn(accountID string) (*conn, error) {
	if c := p.connFor(accountID); c != nil {
		return c, nil
	}
	return nil, provider.ErrNotFound
}

func (p *Provider) SendText(ctx context.Context, accountID, chatID, text, quotedID string) (provider.SendResult, error) {
	if _, err := p.requireConn(accountID); err != nil {
		return provider.SendResult{}, err
	}
	return provider.SendResult{}, errNotImplemented
}

func (p *Provider) StartDirect(ctx context.Context, accountID, phoneE164 string) (string, error) {
	if _, err := p.requireConn(accountID); err != nil {
		return "", err
	}
	return "", errNotImplemented
}

func (p *Provider) React(ctx context.Context, accountID, chatID, messageID, emoji string) error {
	if _, err := p.requireConn(accountID); err != nil {
		return err
	}
	return errNotImplemented
}

func (p *Provider) Edit(ctx context.Context, accountID, chatID, messageID, text string) error {
	if _, err := p.requireConn(accountID); err != nil {
		return err
	}
	return errNotImplemented
}

func (p *Provider) Delete(ctx context.Context, accountID, chatID, messageID string) error {
	if _, err := p.requireConn(accountID); err != nil {
		return err
	}
	return errNotImplemented
}

func (p *Provider) MarkRead(ctx context.Context, accountID, chatID string, messageIDs []string) error {
	if _, err := p.requireConn(accountID); err != nil {
		return err
	}
	return errNotImplemented
}

func (p *Provider) Logout(ctx context.Context, accountID string) error {
	if _, err := p.requireConn(accountID); err != nil {
		return err
	}
	return errNotImplemented
}
