// Package outlook implements the provider contracts against Microsoft Graph.
//
// It is the only package in the tree that knows a Graph URL, an OData query
// parameter, or a Microsoft error code exists.
package outlook

import "github.com/gauravrautela/unified-messaging/internal/provider"

// Name is the identifier recorded on every account this provider owns.
const Name = "OUTLOOK"

// Provider bundles the Microsoft authenticator with the Graph client.
type Provider struct {
	auth   *Auth
	client *Client
}

// New wires the provider.
//
// The token source is usually the account manager, which in turn resolves this
// provider through the registry to refresh grants. That cycle is broken by
// construction order: the manager is built first and learns about the registry
// afterwards, so nothing here needs a fully-formed registry.
func New(auth *Auth, tokens provider.TokenSource) *Provider {
	return &Provider{auth: auth, client: newClient(tokens)}
}

func (p *Provider) Name() string                 { return Name }
func (p *Provider) Auth() provider.Authenticator { return p.auth }
func (p *Provider) Mailbox() provider.Mailbox    { return p.client }
func (p *Provider) Push() provider.Pusher        { return p.client }

// Compile-time proof the implementation satisfies every contract. Without these
// a missing method would surface as a confusing wiring error in main instead of
// a precise one here.
var (
	_ provider.Provider      = (*Provider)(nil)
	_ provider.Authenticator = (*Auth)(nil)
	_ provider.Mailbox       = (*Client)(nil)
	_ provider.Pusher        = (*Client)(nil)
)
