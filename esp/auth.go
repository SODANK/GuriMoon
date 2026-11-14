package esp

// Cookie-based form login for the admin panel. HTTP Basic Auth still works for scripts; the
// in-game browser needs a form and a session cookie on a 200 page.
// See docs/esp-3-notes.md#panel-login-cookie.

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"html"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	evedb "gurimoon/db"
)

const sessionCookieName = "espsess"

// SessionTTL is how long a panel login cookie lives (config.json
// esp.sessionDays). Set before esp.New.
var SessionTTL = 30 * 24 * time.Hour

// PprofEnabled mounts /debug/pprof behind the panel's auth (config.json
// esp.pprof). Set before esp.New.
var PprofEnabled = true

// SessionKeyPath is where the HMAC key that signs login cookies is kept. It is generated on
// first run and reused, so a login outlives a restart. Set before esp.New to relocate it.
var SessionKeyPath = "data/esp-session.key"

type authSession struct {
	user    string
	role    int64
	expires time.Time
}

// loadSessionKey reads the cookie-signing key, generating and persisting one on first run. A
// key that cannot be stored (read-only data dir) falls back to a process-lifetime key, and logins
// then do not survive a restart.
func loadSessionKey() []byte {
	if raw, err := os.ReadFile(SessionKeyPath); err == nil {
		if k, err := hex.DecodeString(strings.TrimSpace(string(raw))); err == nil && len(k) >= 32 {
			return k
		}
		log.Printf("ESP session key %s is malformed, generating a new one", SessionKeyPath)
	}
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		panic(err) // crypto/rand failure is unrecoverable
	}
	if dir := filepath.Dir(SessionKeyPath); dir != "" && dir != "." {
		os.MkdirAll(dir, 0o755)
	}
	if err := os.WriteFile(SessionKeyPath, []byte(hex.EncodeToString(k)), 0o600); err != nil {
		log.Printf("ESP: cannot persist session key to %s (%v) — logins will not survive a restart", SessionKeyPath, err)
	}
	return k
}

// signSession builds a self-contained cookie value: "v1.<user>.<role>.<exp>"
// followed by an HMAC of that prefix. Nothing is stored server-side, so a
// restart does not invalidate the cookie.
func (s *Server) signSession(as authSession) string {
	body := "v1." + base64.RawURLEncoding.EncodeToString([]byte(as.user)) +
		"." + strconv.FormatInt(as.role, 10) +
		"." + strconv.FormatInt(as.expires.Unix(), 10)
	mac := hmac.New(sha256.New, s.sessionKey)
	mac.Write([]byte(body))
	return body + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// parseSession validates a cookie value's signature and expiry.
func (s *Server) parseSession(tok string) (authSession, bool) {
	i := strings.LastIndexByte(tok, '.')
	if i < 0 {
		return authSession{}, false
	}
	body, sig := tok[:i], tok[i+1:]
	want, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil {
		return authSession{}, false
	}
	mac := hmac.New(sha256.New, s.sessionKey)
	mac.Write([]byte(body))
	if !hmac.Equal(want, mac.Sum(nil)) {
		return authSession{}, false
	}
	parts := strings.Split(body, ".")
	if len(parts) != 4 || parts[0] != "v1" {
		return authSession{}, false
	}
	user, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return authSession{}, false
	}
	role, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil {
		return authSession{}, false
	}
	exp, err := strconv.ParseInt(parts[3], 10, 64)
	if err != nil {
		return authSession{}, false
	}
	as := authSession{user: string(user), role: role, expires: time.Unix(exp, 0)}
	if time.Now().After(as.expires) {
		return authSession{}, false
	}
	// The role is signed into the cookie, so tightening esp.minRole or esp.staffRoles locks out
	// issued sessions on the next request.
	if !s.roleAllowed(as.role) {
		return authSession{}, false
	}
	return as, true
}

// roleAllowed is the one admin-panel role test: the role must carry a staff bit
// (esp.staffRoles, evedb.IsStaffRole) and clear the numeric esp.minRole floor.
// See docs/esp-3-notes.md#role-test.
func (s *Server) roleAllowed(role int64) bool {
	return role >= s.minRole && evedb.IsStaffRole(role)
}

// setSessionCookie writes the login cookie. max-age makes it survive the
// browser being closed; path=/ is required by the in-game browser's jar. HttpOnly keeps it from
// page scripts and SameSite=Lax keeps a browser from sending it on a cross-site POST
// (guards.go has the Origin check that covers the rest).
func (s *Server) setSessionCookie(w http.ResponseWriter, as authSession) {
	age := int(time.Until(as.expires).Seconds())
	if age < 0 {
		age = 0
	}
	w.Header().Set("Set-Cookie", fmt.Sprintf("%s=%s; path=/; max-age=%d; HttpOnly; SameSite=Lax",
		sessionCookieName, s.signSession(as), age))
}

// cookieSession returns the session carried by the request, if any.
func (s *Server) cookieSession(r *http.Request) (authSession, bool) {
	c, err := r.Cookie(sessionCookieName)
	if err != nil || c.Value == "" {
		return authSession{}, false
	}
	s.authMu.Lock()
	revokedAt, wasRevoked := s.revoked[c.Value]
	if wasRevoked && time.Now().Before(revokedAt) {
		s.authMu.Unlock()
		return authSession{}, false
	}
	if wasRevoked {
		delete(s.revoked, c.Value)
	}
	s.authMu.Unlock()
	return s.parseSession(c.Value)
}

// cookieAuthed reports whether the request carries a valid session cookie.
func (s *Server) cookieAuthed(r *http.Request) bool {
	_, ok := s.cookieSession(r)
	return ok
}

// handleLogin serves the login form (GET) and processes it (POST).
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		if s.cookieAuthed(r) {
			http.Redirect(w, r, "/", http.StatusFound)
			return
		}
		s.renderLogin(w, r, "")
		return
	}

	user := strings.TrimSpace(r.FormValue("username"))
	pass := r.FormValue("password")
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	_, role, err := evedb.DoLogin(ctx, s.db, user, pass, false, 0)
	if err != nil {
		s.renderLogin(w, r, "Invalid username or password.")
		return
	}
	if !s.roleAllowed(role) {
		s.renderLogin(w, r, "Account role is too low for admin panel access.")
		return
	}

	now := time.Now()
	s.authMu.Lock()
	for t, exp := range s.revoked {
		if now.After(exp) {
			delete(s.revoked, t)
		}
	}
	s.authMu.Unlock()

	home := "http://" + r.Host + "/"
	s.setSessionCookie(w, authSession{user: user, role: role, expires: now.Add(SessionTTL)})
	w.Header().Set("Refresh", "1;URL="+home)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, `<html><body bgcolor=#0a0a0f text=#c8c8c8 link=#6699cc vlink=#6699cc>
<center><br><br>
<h2><font color=#ffcc66>Login successful</font></h2>
<font size=3>Welcome, %s.</font><br><br>
<font size=3><a href="%s">Enter the admin panel</a></font>
</center></body></html>`, html.EscapeString(user), home)
}

// handleLogout drops the session and expires the cookie. Served as a 200
// page (not a redirect) so the in-game browser processes the Set-Cookie.
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	// The cookie is self-validating, so logout blacklists it until it would have expired. The list
	// is in memory; a restart forgets it, and the browser was told to drop the cookie anyway.
	if c, err := r.Cookie(sessionCookieName); err == nil && c.Value != "" {
		if as, ok := s.parseSession(c.Value); ok {
			s.authMu.Lock()
			s.revoked[c.Value] = as.expires
			s.authMu.Unlock()
		}
	}
	w.Header().Set("Set-Cookie", sessionCookieName+"=; path=/; max-age=0")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, `<html><body bgcolor=#0a0a0f text=#c8c8c8 link=#6699cc vlink=#6699cc>
<center><br><br>
<h2><font color=#ffcc66>Logged out</font></h2>
<font size=3><a href="http://%s/login">Log in again</a></font>
</center></body></html>`, r.Host)
}

// renderLogin writes the login form. Plain EVE-HTML subset (font/table/form,
// no CSS or JS) so the in-game browser renders it too.
func (s *Server) renderLogin(w http.ResponseWriter, r *http.Request, errMsg string) {
	action := "http://" + r.Host + "/login"
	var errHTML string
	if errMsg != "" {
		errHTML = `<font size=3 color=#ff6666>` + html.EscapeString(errMsg) + `</font><br><br>`
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, `<html><body bgcolor=#0a0a0f text=#c8c8c8 link=#6699cc vlink=#6699cc>
<center><br><br>
<h2><font color=#ffcc66>GuriMoon &mdash; ESP Admin Panel</font></h2>
<font size=2 color=#888888>Log in with your EVE account</font><br><br>
%s<form action="%s" method="post">
<table cellpadding=4>
<tr><td align=right><font size=3>Username</font></td>
<td><input type="text" name="username" size="24"></td></tr>
<tr><td align=right><font size=3>Password</font></td>
<td><input type="password" name="password" size="24"></td></tr>
<tr><td></td><td><input type="submit" value="Log in"></td></tr>
</table>
</form>
</center></body></html>`, errHTML, action)
}
