package domains

import "context"

type ctxKey int

const userCtxKey ctxKey = iota

// WithUser stamps the authenticated user onto the context. Both the Telegram
// middleware chain and the HTTP session middleware use it, so templates can
// read the viewer with UserFromContext regardless of which side rendered them.
func WithUser(ctx context.Context, u *User) context.Context {
	return context.WithValue(ctx, userCtxKey, u)
}

// UserFromContext returns the context's user, or nil when the caller is
// unauthenticated (no user was fetched, or the row had no Telegram ID).
func UserFromContext(ctx context.Context) *User {
	u, _ := ctx.Value(userCtxKey).(*User)
	return u
}
