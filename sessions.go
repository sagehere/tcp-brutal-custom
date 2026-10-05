//go:build linux

package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

const rememberedSessionLifetime = 30 * 24 * time.Hour

type session struct {
	CSRF     string    `json:"csrf"`
	Expires  time.Time `json:"expires"`
	Remember bool      `json:"-"`
}

type savedSessions struct {
	PasswordFingerprint string             `json:"password_fingerprint"`
	Sessions            map[string]session `json:"sessions"`
}

func sessionDigest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func passwordFingerprint(c config) string {
	return sessionDigest(c.PasswordSalt + ":" + c.PasswordHash)
}

func sessionsPath() string { return filepath.Join(dataDir, "sessions.json") }

func (m *manager) loadSessions() error {
	b, err := os.ReadFile(sessionsPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var saved savedSessions
	if err = json.Unmarshal(b, &saved); err != nil {
		return err
	}
	if saved.PasswordFingerprint != passwordFingerprint(m.cfg) {
		return nil
	}
	if len(saved.Sessions) > 1024 {
		return errors.New("saved session capacity exceeded")
	}
	now := time.Now()
	loaded := make(map[string]session)
	for key, s := range saved.Sessions {
		digest, digestErr := hex.DecodeString(key)
		csrf, csrfErr := hex.DecodeString(s.CSRF)
		if digestErr != nil || len(digest) != 32 || csrfErr != nil || len(csrf) != 16 || s.Expires.IsZero() {
			return errors.New("invalid saved session")
		}
		if now.Before(s.Expires) && !s.Expires.After(now.Add(rememberedSessionLifetime)) {
			s.Remember = true
			loaded[key] = s
		}
	}
	m.sessions = loaded
	return nil
}

// Caller holds m.mu; only remembered sessions are written to disk.
func (m *manager) saveSessions() error {
	saved := savedSessions{PasswordFingerprint: passwordFingerprint(m.cfg), Sessions: make(map[string]session)}
	for key, s := range m.sessions {
		if s.Remember && time.Now().Before(s.Expires) {
			saved.Sessions[key] = s
		}
	}
	b, err := json.Marshal(saved)
	if err != nil {
		return err
	}
	if err = os.MkdirAll(dataDir, 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dataDir, ".sessions-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if err = f.Chmod(0600); err != nil {
		return err
	}
	if _, err = f.Write(b); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), sessionsPath())
}

func sessionCookie(token string) *http.Cookie {
	return &http.Cookie{Name: "session", Value: token, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode}
}

// Caller holds m.mu. Cookie authentication also applies to root peer requests
// for the browser-specific restore and logout endpoints.
func (m *manager) requestSession(r *http.Request) (session, bool) {
	cookie, err := r.Cookie("session")
	if err != nil {
		return session{}, false
	}
	s, ok := m.sessions[sessionDigest(cookie.Value)]
	return s, ok && time.Now().Before(s.Expires)
}

func (m *manager) restoreSession(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	s, ok := m.requestSession(r)
	m.mu.Unlock()
	if !ok {
		bad(w, 401, errors.New("session expired"))
		return
	}
	jsonReply(w, 200, map[string]string{"csrf": s.CSRF})
}

func (m *manager) logoutSession(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.requestSession(r)
	if !ok {
		bad(w, 401, errors.New("session expired"))
		return
	}
	if r.Header.Get("X-CSRF-Token") != s.CSRF {
		bad(w, 403, errors.New("CSRF token required"))
		return
	}
	cookie, _ := r.Cookie("session")
	key := sessionDigest(cookie.Value)
	delete(m.sessions, key)
	if s.Remember {
		if err := m.saveSessions(); err != nil {
			m.sessions[key] = s
			bad(w, 503, fmt.Errorf("revoke panel session: %w", err))
			return
		}
	}
	expired := sessionCookie("")
	expired.MaxAge = -1
	expired.Expires = time.Unix(1, 0)
	http.SetCookie(w, expired)
	jsonReply(w, 200, map[string]bool{"logged_out": true})
}
