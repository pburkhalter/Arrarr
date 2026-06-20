package qbit

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"net/http"
	"sync"
	"time"
)

const sessionCookie = "SID"

type session struct {
	expires time.Time
}

// sessionStore tracks active SID cookies in-process. We don't persist —
// restart kicks all clients back to login, which is exactly what qBit does
// too (sessions are RAM-only there as well).
type sessionStore struct {
	mu     sync.Mutex
	idle   time.Duration
	tokens map[string]session
}

func newSessionStore(idle time.Duration) *sessionStore {
	return &sessionStore{idle: idle, tokens: map[string]session{}}
}

func (s *sessionStore) issue() (string, time.Time) {
	tok := randToken(24)
	exp := time.Now().Add(s.idle)
	s.mu.Lock()
	s.tokens[tok] = session{expires: exp}
	s.mu.Unlock()
	return tok, exp
}

// validate returns true if the SID exists + is not expired, and bumps its
// expiry (sliding window). Returns false (and removes the entry) on expiry.
func (s *sessionStore) validate(tok string) bool {
	if tok == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.tokens[tok]
	if !ok {
		return false
	}
	if time.Now().After(sess.expires) {
		delete(s.tokens, tok)
		return false
	}
	sess.expires = time.Now().Add(s.idle)
	s.tokens[tok] = sess
	return true
}

func (s *sessionStore) revoke(tok string) {
	s.mu.Lock()
	delete(s.tokens, tok)
	s.mu.Unlock()
}

func randToken(nBytes int) string {
	b := make([]byte, nBytes)
	_, _ = rand.Read(b) // crypto/rand never errors in practice
	return base64.RawURLEncoding.EncodeToString(b)
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		writeText(w, http.StatusOK, "Fails.")
		return
	}
	u := r.FormValue("username")
	p := r.FormValue("password")

	// Constant-time compare on both — short-circuiting on username would leak
	// "is this username valid" via timing.
	uOK := subtle.ConstantTimeCompare([]byte(u), []byte(s.username)) == 1
	pOK := subtle.ConstantTimeCompare([]byte(p), []byte(s.password)) == 1
	if !uOK || !pOK {
		s.logger.Warn("qbit login failed", "username", u, "remote", r.RemoteAddr)
		// qBit quirk: invalid creds still return 200 — clients distinguish via
		// the body "Fails." vs "Ok.".
		writeText(w, http.StatusOK, "Fails.")
		return
	}
	tok, exp := s.sessions.issue()
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    tok,
		Path:     "/",
		Expires:  exp,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
	writeText(w, http.StatusOK, "Ok.")
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		s.sessions.revoke(c.Value)
	}
	http.SetCookie(w, &http.Cookie{
		Name:    sessionCookie,
		Value:   "",
		Path:    "/",
		Expires: time.Unix(0, 0),
		MaxAge:  -1,
	})
	writeText(w, http.StatusOK, "")
}

// requireAuth gates every endpoint other than /version, /webapiVersion,
// /auth/login, /auth/logout. When no credentials are configured (empty
// username + password), we let everything through — convenient for dev and
// matches sab/.authenticate semantics.
func (s *Server) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.username == "" && s.password == "" {
			next.ServeHTTP(w, r)
			return
		}
		c, err := r.Cookie(sessionCookie)
		if err != nil || !s.sessions.validate(c.Value) {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}
