package api

import (
	"context"

	"github.com/gauravrautela/unified-messaging/internal/model"
)

type ctxKey int

const (
	ctxDeveloper ctxKey = iota
	ctxAuthKind
)

const (
	authKindAPIKey  = "api_key"
	authKindSession = "session"
)

func withDeveloperCtx(ctx context.Context, d model.Developer, kind string) context.Context {
	ctx = context.WithValue(ctx, ctxDeveloper, d)
	return context.WithValue(ctx, ctxAuthKind, kind)
}

// developerFrom returns the caller resolved by withDeveloper. Handlers under
// /api/v1 can rely on ok being true; it is false only outside that tree.
func developerFrom(ctx context.Context) (model.Developer, bool) {
	d, ok := ctx.Value(ctxDeveloper).(model.Developer)
	return d, ok
}

func authKindFrom(ctx context.Context) string {
	k, _ := ctx.Value(ctxAuthKind).(string)
	return k
}
