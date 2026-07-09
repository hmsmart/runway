package main

import (
	"context"

	"github.com/hmsmart/runway/database/sqlcgen"
)

type ctxKey int

const userCtxKey ctxKey = iota

func WithUser(ctx context.Context, u sqlcgen.User) context.Context {
	return context.WithValue(ctx, userCtxKey, u)
}

func UserFromContext(ctx context.Context) (sqlcgen.User, bool) {
	u, ok := ctx.Value(userCtxKey).(sqlcgen.User)
	return u, ok && u.TgID != nil
}
