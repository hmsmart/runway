package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hmsmart/runway/database"
	"github.com/hmsmart/runway/database/sqlcgen"
	"github.com/hmsmart/runway/domains"
	"github.com/hmsmart/runway/templates"
)

func newTestStore(t *testing.T) *database.Store {
	t.Helper()
	store, err := database.GetStore(context.Background(), filepath.Join(t.TempDir(), "test.db"), time.Minute)
	if err != nil {
		t.Fatalf("open test store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func testUser(t *testing.T) domains.User {
	t.Helper()
	tgID := int64(12345)
	name := "Test"
	u := domains.NewUser(sqlcgen.User{ID: "user-1", TgID: &tgID, TgFirstName: &name, Active: true})
	if u == nil {
		t.Fatal("test user should not be nil")
	}
	return *u
}

func postConfirm(h http.Handler, token string, cookies []*http.Cookie) *httptest.ResponseRecorder {
	form := url.Values{"token": {token}}
	req := httptest.NewRequest("POST", "/dash", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestMagicLinkSession walks the /dash magic-link flow: the GET shows the
// confirm step without spending the token (so link previews can't burn it),
// the confirm POST burns it and sets a session cookie, and that cookie opens
// the gated dashboard.
func TestMagicLinkSession(t *testing.T) {
	store := newTestStore(t)
	user := testUser(t)
	store.TGTokens.Set("magic", user, time.Minute)

	// GETs — even repeated, as a previewer would — leave the token alive.
	dash := withSessionUser(store, handleDash(store))
	for range 2 {
		rec := httptest.NewRecorder()
		dash.ServeHTTP(rec, httptest.NewRequest("GET", "/dash?token=magic", nil))
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `name="token"`) {
			t.Fatalf("want confirm page, got %d: %.200s", rec.Code, rec.Body.String())
		}
		if !store.TGTokens.Has("magic") {
			t.Fatal("GET must not consume the magic token")
		}
	}

	// The confirm POST consumes the token and signs the browser in.
	confirm := withSessionUser(store, handleMagicConfirm(store, "/dashboard"))
	rec := postConfirm(confirm, "magic", nil)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/dashboard" {
		t.Fatalf("want 303 to /dashboard, got %d to %q", rec.Code, rec.Header().Get("Location"))
	}
	cookies := rec.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != sessionCookieName {
		t.Fatalf("want one %s cookie, got %v", sessionCookieName, cookies)
	}
	if store.TGTokens.Has("magic") {
		t.Fatal("confirm POST should burn the magic token")
	}

	// The cookie should now open the gated dashboard.
	gated := withSessionUser(store, requireSession(handleDashboard))
	req := httptest.NewRequest("GET", "/dashboard", nil)
	req.AddCookie(cookies[0])
	rec = httptest.NewRecorder()
	gated.ServeHTTP(rec, req)
	// The dashboard lowercases the greeting as a style choice.
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "welcome back, test") {
		t.Fatalf("want dashboard greeting, got %d: %.200s", rec.Code, rec.Body.String())
	}

	// Revisiting the spent magic URL with the session skips to the dashboard;
	// a session-holding double-tap of confirm passes through too.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("GET", "/dash?token=magic", nil)
	req.AddCookie(cookies[0])
	dash.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/dashboard" {
		t.Fatalf("spent token with session: want 303 to /dashboard, got %d to %q", rec.Code, rec.Header().Get("Location"))
	}
	if rec := postConfirm(confirm, "magic", cookies); rec.Code != http.StatusSeeOther {
		t.Fatalf("confirm replay with session: want 303, got %d", rec.Code)
	}

	// Without a session, the spent token is dead in both handlers.
	rec = httptest.NewRecorder()
	dash.ServeHTTP(rec, httptest.NewRequest("GET", "/dash?token=magic", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("spent token GET: want 400, got %d", rec.Code)
	}
	if rec := postConfirm(confirm, "magic", nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("spent token POST: want 400, got %d", rec.Code)
	}
}

// TestLogin checks the login page tells anonymous visitors how to sign in
// and bounces already-authenticated ones to the dashboard.
func TestLogin(t *testing.T) {
	store := newTestStore(t)
	user := testUser(t)
	h := withSessionUser(store, http.HandlerFunc(handleLogin))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/login", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "/dash") {
		t.Fatalf("want login page mentioning /dash, got %d: %.200s", rec.Code, rec.Body.String())
	}

	srec := httptest.NewRecorder()
	createSession(srec, store, user)
	req := httptest.NewRequest("GET", "/login", nil)
	req.AddCookie(srec.Result().Cookies()[0])
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/dashboard" {
		t.Fatalf("signed-in /login: want 303 to /dashboard, got %d to %q", rec.Code, rec.Header().Get("Location"))
	}
}

func TestRequireSessionRejectsAnonymous(t *testing.T) {
	store := newTestStore(t)
	gated := withSessionUser(store, requireSession(handleDashboard))
	rec := httptest.NewRecorder()
	gated.ServeHTTP(rec, httptest.NewRequest("GET", "/dashboard", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", rec.Code)
	}
}

// TestNavAuthenticated checks the shared nav only offers the signed-in
// entries (dashboard, link, profile picture) to a session-carrying context.
func TestNavAuthenticated(t *testing.T) {
	user := testUser(t)

	var anon strings.Builder
	if err := templates.IndexPage().Render(context.Background(), &anon); err != nil {
		t.Fatalf("render: %v", err)
	}
	for _, frag := range []string{`href="/dashboard"`, `href="/link"`, `href="/logout"`} {
		if strings.Contains(anon.String(), frag) {
			t.Errorf("anonymous nav should not contain %s", frag)
		}
	}
	for _, frag := range []string{`href="/login"`, `src="/profilepic"`} {
		if !strings.Contains(anon.String(), frag) {
			t.Errorf("anonymous nav should contain %s", frag)
		}
	}

	var authed strings.Builder
	ctx := domains.WithUser(context.Background(), &user)
	if err := templates.IndexPage().Render(ctx, &authed); err != nil {
		t.Fatalf("render: %v", err)
	}
	for _, frag := range []string{`href="/dashboard"`, `href="/link"`, `href="/logout"`, `src="/profilepic"`} {
		if !strings.Contains(authed.String(), frag) {
			t.Errorf("authenticated nav should contain %s", frag)
		}
	}
}

// TestProfilePic checks /profilepic resolves the avatar from the session:
// the signed-in user's photo, the default for everyone else.
func TestProfilePic(t *testing.T) {
	store := newTestStore(t)
	user := testUser(t)
	realPhoto := domains.NewPhoto(1, 1, []byte("real-photo-bytes"), "p1")
	store.TGPhotos.Set(user.ID(), *realPhoto, time.Minute)

	h := withSessionUser(store, handleProfilePic(store))
	get := func(cookie *http.Cookie) string {
		req := httptest.NewRequest("GET", "/profilepic", nil)
		if cookie != nil {
			req.AddCookie(cookie)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /profilepic: want 200, got %d", rec.Code)
		}
		return rec.Body.String()
	}

	if strings.Contains(get(nil), "real-photo-bytes") {
		t.Fatal("anonymous request must not receive a real photo")
	}

	rec := httptest.NewRecorder()
	createSession(rec, store, user)
	cookie := rec.Result().Cookies()[0]
	if !strings.Contains(get(cookie), "real-photo-bytes") {
		t.Fatal("session user should receive their own photo")
	}

	// Logout kills the session server-side and expires the cookie; the same
	// browser cookie is now worthless.
	logout := withSessionUser(store, handleLogout(store))
	req := httptest.NewRequest("GET", "/logout", nil)
	req.AddCookie(cookie)
	lrec := httptest.NewRecorder()
	logout.ServeHTTP(lrec, req)
	if lrec.Code != http.StatusSeeOther || lrec.Header().Get("Location") != "/" {
		t.Fatalf("logout: want 303 to /, got %d to %q", lrec.Code, lrec.Header().Get("Location"))
	}
	if cleared := lrec.Result().Cookies(); len(cleared) != 1 || cleared[0].MaxAge >= 0 {
		t.Fatalf("logout should expire the session cookie, got %v", cleared)
	}
	if strings.Contains(get(cookie), "real-photo-bytes") {
		t.Fatal("stale cookie after logout must not resolve to a photo")
	}
}
