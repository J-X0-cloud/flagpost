package ui

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	sessionCookie = "flagpost_session"
	sessionTTL    = 12 * time.Hour
)

// sessions issues and verifies stateless session cookies. A cookie is
// "<expiry unix>.<hex HMAC-SHA256(secret, expiry)>", so the server keeps no
// session table and rotating the admin token logs everyone out.
type sessions struct {
	secret []byte
	now    func() time.Time
}

func (s *sessions) mac(parts ...string) string {
	m := hmac.New(sha256.New, s.secret)
	m.Write([]byte(strings.Join(parts, "|")))
	return hex.EncodeToString(m.Sum(nil))
}

func (s *sessions) issue() (value string, expires time.Time) {
	expires = s.now().Add(sessionTTL)
	exp := strconv.FormatInt(expires.Unix(), 10)
	return exp + "." + s.mac("session", exp), expires
}

func (s *sessions) valid(value string) bool {
	exp, sig, ok := strings.Cut(value, ".")
	if !ok {
		return false
	}
	n, err := strconv.ParseInt(exp, 10, 64)
	if err != nil || s.now().Unix() >= n {
		return false
	}
	return hmac.Equal([]byte(sig), []byte(s.mac("session", exp)))
}

// csrfToken binds a form token to the session cookie, so a cross-site
// page that cannot read the cookie cannot forge it.
func (s *sessions) csrfToken(session string) string { return s.mac("csrf", session) }

func (s *sessions) checkCSRF(session, token string) bool {
	return token != "" && hmac.Equal([]byte(token), []byte(s.csrfToken(session)))
}

func (s *sessions) cookie(r *http.Request) string {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return ""
	}
	return c.Value
}

func (s *sessions) set(w http.ResponseWriter, r *http.Request) {
	value, expires := s.issue()
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: value, Path: "/ui", Expires: expires,
		HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: r.TLS != nil,
	})
}

func (s *sessions) clear(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/ui", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteStrictMode})
}
