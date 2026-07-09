package main

import (
	"context"

	"github.com/hmsmart/runway/domains"
)

type ctxKey int

const userCtxKey ctxKey = iota

func WithUser(ctx context.Context, u *domains.User) context.Context {
	return context.WithValue(ctx, userCtxKey, u)
}

// UserFromContext returns the sender's user, or nil when the sender is
// unregistered (no user was fetched, or the row had no Telegram ID).
func UserFromContext(ctx context.Context) *domains.User {
	u, _ := ctx.Value(userCtxKey).(*domains.User)
	return u
}
