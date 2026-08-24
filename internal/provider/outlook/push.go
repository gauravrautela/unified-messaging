package outlook

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"time"

	"github.com/gauravrautela/unified-messaging/internal/provider"
)

// watchedResource is the whole mailbox rather than just the inbox, so moves,
// sends and deletions produce wakeups too.
const watchedResource = "me/messages"
const watchedChangeTypes = "created,updated,deleted"

// maxSubscriptionAge is how far ahead we ask Graph to keep an Outlook message
// subscription alive. The documented ceiling is 10,080 minutes (under 7 days);
// staying inside it means a slightly slow renewal never overshoots.
const maxSubscriptionAge = 6 * 24 * time.Hour

// renewBefore is the remaining-lifetime threshold that triggers renewal.
// Generous on purpose: a lapsed subscription stops delivering push entirely and
// only the polling fallback notices.
const renewBefore = 24 * time.Hour

func (c *Client) RenewBefore() time.Duration { return renewBefore }

type graphSubscription struct {
	ID                 string `json:"id"`
	Resource           string `json:"resource"`
	ChangeType         string `json:"changeType"`
	NotificationURL    string `json:"notificationUrl"`
	LifecycleURL       string `json:"lifecycleNotificationUrl,omitempty"`
	ExpirationDateTime string `json:"expirationDateTime"`
	ClientState        string `json:"clientState,omitempty"`
	TLSVersion         string `json:"latestSupportedTlsVersion,omitempty"`
}

func (g graphSubscription) toModel(clientState string) provider.Subscription {
	return provider.Subscription{
		ID:          g.ID,
		Resource:    g.Resource,
		ClientState: clientState,
		ExpiresAt:   parseTime(g.ExpirationDateTime),
	}
}

// Create asks Graph to push change notifications for the mailbox.
//
// Graph validates the notification URL synchronously before returning: it POSTs
// with a ?validationToken query parameter and requires a 200 echoing that token
// as text/plain. If our endpoint is not publicly reachable this fails with 400
// immediately rather than degrading quietly.
func (c *Client) Create(ctx context.Context, accountID string, cfg provider.PushConfig) (provider.Subscription, error) {
	body := graphSubscription{
		Resource:           watchedResource,
		ChangeType:         watchedChangeTypes,
		NotificationURL:    cfg.NotificationURL,
		LifecycleURL:       cfg.LifecycleURL,
		ClientState:        cfg.ClientState,
		ExpirationDateTime: time.Now().Add(maxSubscriptionAge).UTC().Format(time.RFC3339),
		TLSVersion:         "v1_2",
	}
	var out graphSubscription
	if err := c.do(ctx, accountID, request{
		method: http.MethodPost,
		url:    "/subscriptions",
		body:   body,
		out:    &out,
	}); err != nil {
		return provider.Subscription{}, err
	}
	return out.toModel(cfg.ClientState), nil
}

func (c *Client) Renew(ctx context.Context, accountID, subscriptionID string) (provider.Subscription, error) {
	var out graphSubscription
	if err := c.do(ctx, accountID, request{
		method: http.MethodPatch,
		url:    "/subscriptions/" + url.PathEscape(subscriptionID),
		body: map[string]any{
			"expirationDateTime": time.Now().Add(maxSubscriptionAge).UTC().Format(time.RFC3339),
		},
		out: &out,
	}); err != nil {
		return provider.Subscription{}, err
	}
	return out.toModel(""), nil
}

func (c *Client) Delete(ctx context.Context, accountID, subscriptionID string) error {
	return c.do(ctx, accountID, request{
		method: http.MethodDelete,
		url:    "/subscriptions/" + url.PathEscape(subscriptionID),
	})
}

// List reports what Graph currently holds for this account. It is what lets the
// core recover from a duplicate-subscription rejection: Graph refuses a second
// subscription for the same (changeType, resource), which happens whenever our
// local record is lost but the remote one is still alive.
func (c *Client) List(ctx context.Context, accountID string) ([]provider.Subscription, error) {
	var page struct {
		Value []graphSubscription `json:"value"`
	}
	if err := c.do(ctx, accountID, request{
		method: http.MethodGet,
		url:    "/subscriptions",
		out:    &page,
	}); err != nil {
		return nil, err
	}
	out := make([]provider.Subscription, 0, len(page.Value))
	for _, g := range page.Value {
		// The clientState of a subscription we did not create is unknowable;
		// Graph never returns it on a read.
		out = append(out, g.toModel(""))
	}
	return out, nil
}

type graphNotificationPayload struct {
	Value []struct {
		SubscriptionID string `json:"subscriptionId"`
		ClientState    string `json:"clientState"`
		ChangeType     string `json:"changeType"`
		Resource       string `json:"resource"`
		LifecycleEvent string `json:"lifecycleEvent"`
	} `json:"value"`
}

// ParseNotifications decodes an inbound Graph push payload, translating
// Microsoft's lifecycle vocabulary into the actions the core understands.
func (c *Client) ParseNotifications(raw []byte) ([]provider.Notification, error) {
	var payload graphNotificationPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, err
	}
	out := make([]provider.Notification, 0, len(payload.Value))
	for _, n := range payload.Value {
		out = append(out, provider.Notification{
			SubscriptionID: n.SubscriptionID,
			ClientState:    n.ClientState,
			Lifecycle:      lifecycleAction(n.LifecycleEvent),
		})
	}
	return out, nil
}

// ValidationResponse implements Graph's endpoint validation handshake: it POSTs
// with a ?validationToken query parameter and requires a 200 echoing that token
// back as text/plain, within seconds, before it will create a subscription.
func (c *Client) ValidationResponse(query url.Values) (string, bool) {
	token := query.Get("validationToken")
	return token, token != ""
}

func lifecycleAction(event string) provider.LifecycleAction {
	switch event {
	case "":
		return provider.LifecycleNone
	case "reauthorizationRequired":
		return provider.LifecycleReauthorize
	case "subscriptionRemoved", "missed":
		// "missed" means notifications were dropped, so the local store may have
		// gaps — the subscription has to be rebuilt and the account resynced.
		return provider.LifecycleRecreate
	default:
		return provider.LifecycleNone
	}
}
