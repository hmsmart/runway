package main

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/hmsmart/runway/database"
	"github.com/hmsmart/runway/domains"
	"github.com/jellydator/ttlcache/v3"
)

const sessionCookieName = "runway_session"

// createSession mints a browser session for user and sets its cookie. Called
// exactly when a Telegram magic link is consumed (/link or /dash), so holding
// the cookie proves the same thing holding the single-use token did.
func createSession(w http.ResponseWriter, store *database.Store, user domains.User) {
	token := RandomToken(32, Base64)
	store.Sessions.Set(token, user, ttlcache.DefaultTTL)
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    token,
		Path:     "/",
		MaxAge:   int(database.SessionTTL / time.Second),
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
	})
	slog.Info("session created", "user", user.Username())
}

// withSessionUser resolves the session cookie to a user and stamps it onto
// the request context; templates and handlers read it back with
// domains.UserFromContext. Requests without a live session pass through
// unauthenticated — page-level gating is requireSession's job.
func withSessionUser(store *database.Store, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if c, err := r.Cookie(sessionCookieName); err == nil {
			if cached := store.Sessions.Get(c.Value); cached != nil {
				user := cached.Value()
				r = r.WithContext(domains.WithUser(r.Context(), &user))
			}
		}
		next.ServeHTTP(w, r)
	})
}

// requireSession rejects requests whose context carries no session user.
func requireSession(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if domains.UserFromContext(r.Context()) == nil {
			httpError(r.Context(), w, clientIP(r), http.StatusUnauthorized,
				"not authorized — ask the bot for a fresh link", "path", r.URL.Path)
			return
		}
		next(w, r)
	}
}
