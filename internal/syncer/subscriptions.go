package syncer

import (
	"context"
	"crypto/subtle"
	"errors"
	"time"

	"github.com/gauravrautela/unified-messaging/internal/accounts"
	"github.com/gauravrautela/unified-messaging/internal/logx"
	"github.com/gauravrautela/unified-messaging/internal/model"
	"github.com/gauravrautela/unified-messaging/internal/provider"
	"github.com/gauravrautela/unified-messaging/internal/store"
)

// pusherFor returns the account's push implementation, or nil when the provider
// has none. IMAP, for instance, would simply never implement Pusher, and the
// polling loop carries the whole load for those accounts.
func (s *Syncer) pusherFor(acct model.Account) (provider.Pusher, error) {
	p, err := s.registry.Get(acct.Provider)
	if err != nil {
		return nil, err
	}
	return p.Push(), nil
}

func (s *Syncer) subscriptionLoop(ctx context.Context) {
	// Reconcile immediately on boot: anything that lapsed while we were down
	// needs replacing before push can be trusted at all.
	s.reconcileSubscriptions(ctx)

	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.reconcileSubscriptions(ctx)
		}
	}
}

func (s *Syncer) reconcileSubscriptions(ctx context.Context) {
	// The context gets the untagged logger: ids travel on it, components do not.
	ctx = logx.With(ctx, s.base)
	accts, err := s.store.ListAllAccounts()
	if err != nil {
		s.log.Error("listing accounts for subscription reconcile", "err", err)
		return
	}
	s.log.Debug("subscription reconcile", "accounts", len(accts))
	for _, a := range accts {
		if a.Status != model.AccountOK {
			continue
		}
		p, err := s.registry.Get(a.Provider)
		if err != nil || p.Kind() != model.AccountKindMail {
			s.log.Debug("skipping non-mail account", "account_id", a.ID, "provider", a.Provider)
			continue
		}
		if err := s.EnsureSubscription(ctx, a.ID); err != nil {
			if errors.Is(err, provider.ErrReauthRequired) {
				continue
			}
			s.log.Error("ensuring subscription", "account_id", a.ID, "err", err)
		}
	}
}

// EnsureSubscription guarantees a live push subscription, renewing one close to
// expiry and creating one otherwise. It is a no-op for providers without push.
func (s *Syncer) EnsureSubscription(ctx context.Context, accountID string) error {
	if s.opts.PublicBaseURL == "" {
		return nil
	}
	acct, err := s.store.GetAnyAccount(accountID)
	if err != nil {
		return err
	}
	pusher, err := s.pusherFor(acct)
	if err != nil {
		return err
	}
	if pusher == nil {
		return nil
	}
	// clientState is deliberately absent from every line below: it is the shared
	// secret that authenticates inbound notifications.
	//
	// This is where the account first becomes known on this path, so the ids go
	// onto the context logger here and nowhere downstream; component is added
	// only to the local logger.
	ctx = logx.With(ctx, logx.From(ctx).With("account_id", accountID,
		"developer_id", acct.DeveloperID, "provider", acct.Provider))
	log := logx.From(ctx).With("component", "syncer")

	existing, err := s.store.SubscriptionsForAccount(accountID)
	if err != nil {
		return err
	}
	for _, sub := range existing {
		if time.Until(sub.ExpiresAt) > pusher.RenewBefore() {
			log.Debug("subscription decision", "decision", "healthy",
				"subscription_id", sub.ID, "resource", sub.Resource, "expires_at", sub.ExpiresAt)
			return nil // still healthy
		}
		log.Debug("subscription decision", "decision", "renew",
			"subscription_id", sub.ID, "resource", sub.Resource, "expires_at", sub.ExpiresAt)
		renewed, err := pusher.Renew(ctx, accountID, sub.ID)
		if err == nil {
			log.Info("renewed subscription",
				"subscription_id", sub.ID, "resource", sub.Resource, "expires_at", renewed.ExpiresAt)
			return s.store.SaveSubscription(store.Subscription{
				ID: sub.ID, AccountID: accountID, Resource: sub.Resource,
				ClientState: sub.ClientState, ExpiresAt: renewed.ExpiresAt,
			})
		}
		// Providers forget subscriptions they have expired; drop ours and make
		// a fresh one.
		log.Warn("renewal failed, recreating", "subscription_id", sub.ID, "err", err)
		log.Debug("subscription decision", "decision", "delete",
			"subscription_id", sub.ID, "reason", "renew failed")
		_ = s.store.DeleteSubscription(sub.ID)
	}

	log.Debug("subscription decision", "decision", "create", "existing", len(existing))
	return s.createSubscription(ctx, accountID, acct.Provider, pusher, false)
}

func (s *Syncer) createSubscription(ctx context.Context, accountID, providerName string, pusher provider.Pusher, retried bool) error {
	// clientState is the shared secret authenticating inbound notifications.
	// Providers generally cannot send custom headers, so this value is the only
	// thing proving a POST to our notification endpoint is genuine.
	secret, err := accounts.NewID("cs")
	if err != nil {
		return err
	}

	sub, err := pusher.Create(ctx, accountID, provider.PushConfig{
		NotificationURL: s.opts.notificationURL(providerName),
		LifecycleURL:    s.opts.lifecycleURL(providerName),
		ClientState:     secret,
	})
	if err != nil {
		if errors.Is(err, provider.ErrSubscriptionExists) {
			if retried {
				// We already deleted everything the provider listed and tried
				// again once; a second "duplicate" means either Delete silently
				// failed to take effect or the provider is lying to us. Either
				// way, looping forever is worse than surfacing it.
				logx.From(ctx).With("component", "syncer").Error(
					"provider still reports an existing subscription after replacing it",
					"account_id", accountID)
				return errors.New("syncer: provider still reports an existing subscription after replacing it")
			}
			// A subscription we lost track of is still alive upstream. Its
			// clientState is one we never knew, so notifications for it could
			// only ever be checked by subscription ID — not good enough to
			// trust, since anyone who learns that ID could force a sync.
			// Replace it with one we mint ourselves instead of adopting it blind.
			return s.adoptExisting(ctx, accountID, providerName, pusher, true)
		}
		return err
	}

	logx.From(ctx).With("component", "syncer").Info("created subscription",
		"subscription_id", sub.ID, "resource", sub.Resource, "expires_at", sub.ExpiresAt)
	return s.store.SaveSubscription(store.Subscription{
		ID: sub.ID, AccountID: accountID, Resource: sub.Resource,
		ClientState: secret, ExpiresAt: sub.ExpiresAt,
	})
}

// adoptExisting replaces every subscription the provider reports as already
// registered for this account. Trusting one of them in place — as this used
// to do — meant storing it with no clientState, which made HandleNotifications
// accept any inbound POST that merely named that subscription ID: anyone who
// learned or guessed the ID could force a sync with no secret at all. Deleting
// and recreating costs one extra round trip but ensures every stored
// subscription carries a clientState only we ever knew.
func (s *Syncer) adoptExisting(ctx context.Context, accountID, providerName string, pusher provider.Pusher, retried bool) error {
	remote, err := pusher.List(ctx, accountID)
	if err != nil {
		return err
	}
	if len(remote) == 0 {
		return errors.New("syncer: provider reported a duplicate subscription but did not list it")
	}
	log := logx.From(ctx).With("component", "syncer")
	for _, r := range remote {
		if err := pusher.Delete(ctx, accountID, r.ID); err != nil {
			log.Warn("deleting pre-existing subscription before replacing it",
				"account_id", accountID, "subscription_id", r.ID, "err", err)
		}
	}
	log.Info("replaced pre-existing subscription", "account_id", accountID, "count", len(remote))
	return s.createSubscription(ctx, accountID, providerName, pusher, retried)
}

// HandleNotifications processes an inbound push payload from a named provider.
//
// The payload is not treated as data. It only says *something* changed; the
// incremental walk determines what. That keeps push and polling on one code
// path with one set of dedupe rules.
func (s *Syncer) HandleNotifications(ctx context.Context, providerName string, raw []byte) error {
	pusher, err := s.pusherByName(providerName)
	if err != nil {
		return err
	}
	notifications, err := pusher.ParseNotifications(raw)
	if err != nil {
		return err
	}

	log := s.log.With("provider", providerName)
	log.Debug("notifications received", "bytes", len(raw), "notifications", len(notifications))

	for _, n := range notifications {
		sub, err := s.store.GetSubscription(n.SubscriptionID)
		if err != nil {
			// Unknown subscriptions are expected noise after a database reset.
			log.Warn("notification for unknown subscription",
				"subscription_id", n.SubscriptionID)
			continue
		}
		// A blank stored clientState means we never had one to check — which,
		// since adoptExisting no longer stores subscriptions that way, should
		// not happen for anything created going forward; reject it outright
		// rather than falling back to trusting the subscription ID alone.
		// The comparison itself is constant-time: this string is a bearer
		// secret, and a timing side-channel on it would undo the point of
		// having one.
		if sub.ClientState == "" ||
			subtle.ConstantTimeCompare([]byte(sub.ClientState), []byte(n.ClientState)) != 1 {
			log.Warn("rejecting notification: clientState mismatch",
				"subscription_id", n.SubscriptionID)
			continue
		}

		if n.Lifecycle != provider.LifecycleNone {
			if err := s.handleLifecycle(ctx, sub, n.Lifecycle); err != nil {
				log.Error("handling lifecycle notification",
					"subscription_id", n.SubscriptionID, "err", err)
			}
			continue
		}
		log.Debug("notification decision", "decision", "wake",
			"subscription_id", n.SubscriptionID, "account_id", sub.AccountID)
		s.Wake(sub.AccountID)
	}
	return nil
}

// PusherByName exposes a provider's push implementation to the HTTP layer, so
// it can answer endpoint-validation challenges. Returns nil when the provider
// exists but has no push mechanism.
func (s *Syncer) PusherByName(providerName string) (provider.Pusher, error) {
	return s.pusherByName(providerName)
}

func (s *Syncer) pusherByName(providerName string) (provider.Pusher, error) {
	p, err := s.registry.Get(providerName)
	if err != nil {
		return nil, err
	}
	pusher := p.Push()
	if pusher == nil {
		return nil, errors.New("syncer: provider " + providerName + " does not support push notifications")
	}
	return pusher, nil
}

// handleLifecycle responds to a provider's out-of-band subscription warnings.
// Without this a subscription can quietly stop delivering, and only the poll
// would ever notice.
func (s *Syncer) handleLifecycle(ctx context.Context, sub store.Subscription, action provider.LifecycleAction) error {
	acct, err := s.store.GetAnyAccount(sub.AccountID)
	if err != nil {
		return err
	}
	pusher, err := s.pusherFor(acct)
	if err != nil || pusher == nil {
		return err
	}
	s.log.Info("lifecycle notification",
		"subscription_id", sub.ID, "account_id", sub.AccountID, "action", action)

	switch action {
	case provider.LifecycleReauthorize:
		// The provider wants proof the delegated grant is still good. Forcing a
		// token refresh and renewing is exactly that proof.
		if _, err := s.accts.AccessToken(ctx, sub.AccountID, true); err != nil {
			return err
		}
		_, err := pusher.Renew(ctx, sub.AccountID, sub.ID)
		return err

	case provider.LifecycleRecreate:
		_ = s.store.DeleteSubscription(sub.ID)
		if err := s.EnsureSubscription(ctx, sub.AccountID); err != nil {
			return err
		}
		s.Wake(sub.AccountID)
		return nil
	}
	return nil
}

// RemoveSubscriptions best-effort deletes an account's subscriptions upstream,
// so disconnecting does not leave a provider pushing at us forever.
func (s *Syncer) RemoveSubscriptions(ctx context.Context, accountID string) {
	acct, err := s.store.GetAnyAccount(accountID)
	if err != nil {
		return
	}
	pusher, err := s.pusherFor(acct)
	if err != nil || pusher == nil {
		return
	}
	subs, err := s.store.SubscriptionsForAccount(accountID)
	if err != nil {
		return
	}
	ctx = logx.With(ctx, logx.From(ctx).With("account_id", accountID))
	log := logx.From(ctx).With("component", "syncer")
	for _, sub := range subs {
		log.Debug("subscription decision", "decision", "delete",
			"subscription_id", sub.ID, "resource", sub.Resource,
			"expires_at", sub.ExpiresAt, "reason", "account disconnected")
		if err := pusher.Delete(ctx, accountID, sub.ID); err != nil {
			log.Warn("deleting subscription upstream", "subscription_id", sub.ID, "err", err)
		}
		_ = s.store.DeleteSubscription(sub.ID)
	}
}
