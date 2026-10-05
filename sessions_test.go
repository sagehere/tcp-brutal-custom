//go:build linux

package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func panelTestManager(t *testing.T) *manager {
	t.Helper()
	oldData, oldConfig, oldPassword := dataDir, configDir, panelPasswordFile
	dir := t.TempDir()
	dataDir, configDir, panelPasswordFile = filepath.Join(dir, "data"), filepath.Join(dir, "config"), filepath.Join(dir, "config", "panel-password")
	t.Cleanup(func() { dataDir, configDir, panelPasswordFile = oldData, oldConfig, oldPassword })
	cfg := config{WebHost: "127.0.0.1", WebPort: 8080}
	if err := setPassword(&cfg, "test-password"); err != nil {
		t.Fatal(err)
	}
	return &manager{cfg: cfg, sessions: map[string]session{}, attempts: map[string]attempt{}}
}

func panelRequest(m *manager, method, path, body string, cookie *http.Cookie, csrf string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if cookie != nil {
		r.AddCookie(cookie)
	}
	r.Header.Set("X-CSRF-Token", csrf)
	w := httptest.NewRecorder()
	m.routes().ServeHTTP(w, r)
	return w
}

func panelLogin(t *testing.T, m *manager, remember bool) (*http.Cookie, string) {
	t.Helper()
	body := `{"password":"test-password"}`
	if remember {
		body = `{"password":"test-password","remember_me":true}`
	}
	w := panelRequest(m, "POST", "/api/v1/login", body, nil, "")
	if w.Code != 200 || len(w.Result().Cookies()) != 1 {
		t.Fatalf("login status=%d body=%s", w.Code, w.Body)
	}
	var reply struct{ CSRF string }
	if err := json.Unmarshal(w.Body.Bytes(), &reply); err != nil {
		t.Fatal(err)
	}
	return w.Result().Cookies()[0], reply.CSRF
}

func TestPanelSessionLifecycle(t *testing.T) {
	for _, remember := range []bool{false, true} {
		t.Run(map[bool]string{false: "temporary", true: "remembered"}[remember], func(t *testing.T) {
			m := panelTestManager(t)
			cookie, csrf := panelLogin(t, m, remember)
			if !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode || cookie.Path != "/" {
				t.Fatalf("cookie protection=%+v", cookie)
			}
			s := m.sessions[sessionDigest(cookie.Value)]
			lifetime := 8 * time.Hour
			if remember {
				lifetime = rememberedSessionLifetime
				if cookie.MaxAge != 30*86400 || cookie.Expires.Unix() != s.Expires.Unix() {
					t.Fatalf("remembered cookie=%+v", cookie)
				}
				b, err := os.ReadFile(sessionsPath())
				info, statErr := os.Stat(sessionsPath())
				if err != nil || statErr != nil || strings.Contains(string(b), cookie.Value) || info.Mode().Perm() != 0600 {
					t.Fatalf("session file is not protected: read=%v stat=%v", err, statErr)
				}
			} else if cookie.MaxAge != 0 || !cookie.Expires.IsZero() {
				t.Fatalf("temporary cookie=%+v", cookie)
			}
			if remaining := time.Until(s.Expires); remaining > lifetime || remaining < lifetime-time.Minute {
				t.Fatalf("session lifetime=%v want=%v", remaining, lifetime)
			}
			if w := panelRequest(m, "GET", "/api/v1/session", "", cookie, ""); w.Code != 200 || !strings.Contains(w.Body.String(), csrf) {
				t.Fatalf("restore status=%d body=%s", w.Code, w.Body)
			}
			r := httptest.NewRequest("POST", "/api/v1/ports", nil)
			r.AddCookie(cookie)
			if w := httptest.NewRecorder(); m.authorized(w, r) || w.Code != 403 {
				t.Fatal("protected API accepted missing CSRF")
			}
			r.Header.Set("X-CSRF-Token", csrf)
			if !m.authorized(httptest.NewRecorder(), r) {
				t.Fatal("restored CSRF cannot authorize protected API")
			}
			restarted := &manager{cfg: m.cfg, sessions: map[string]session{}}
			if err := restarted.loadSessions(); err != nil {
				t.Fatal(err)
			}
			want := 401
			if remember {
				want = 200
			}
			if w := panelRequest(restarted, "GET", "/api/v1/session", "", cookie, ""); w.Code != want {
				t.Fatalf("restart status=%d want=%d", w.Code, want)
			}
			if w := panelRequest(m, "POST", "/api/v1/logout", "{}", cookie, "wrong"); w.Code != 403 {
				t.Fatalf("logout without CSRF status=%d", w.Code)
			}
			w := panelRequest(m, "POST", "/api/v1/logout", "{}", cookie, csrf)
			if w.Code != 200 || w.Result().Cookies()[0].MaxAge != -1 {
				t.Fatalf("logout status=%d body=%s", w.Code, w.Body)
			}
			if w := panelRequest(m, "GET", "/api/v1/session", "", cookie, ""); w.Code != 401 {
				t.Fatalf("revoked session status=%d", w.Code)
			}
			restarted.sessions = map[string]session{}
			if err := restarted.loadSessions(); err != nil || len(restarted.sessions) != 0 {
				t.Fatalf("logout persisted: sessions=%v err=%v", restarted.sessions, err)
			}
		})
	}
}

func TestPanelSessionExpiryAndValidation(t *testing.T) {
	m := panelTestManager(t)
	cookie, _ := panelLogin(t, m, true)
	key := sessionDigest(cookie.Value)
	s := m.sessions[key]
	s.Expires = time.Now().Add(-time.Second)
	m.sessions[key] = s
	if w := panelRequest(m, "GET", "/api/v1/session", "", cookie, ""); w.Code != 401 {
		t.Fatalf("expired session status=%d", w.Code)
	}
	b, err := json.Marshal(savedSessions{PasswordFingerprint: passwordFingerprint(m.cfg), Sessions: m.sessions})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sessionsPath(), b, 0600); err != nil {
		t.Fatal(err)
	}
	m.sessions = map[string]session{}
	if err := m.loadSessions(); err != nil || len(m.sessions) != 0 {
		t.Fatalf("expired session restored: %v", err)
	}
	r := httptest.NewRequest("GET", "/api/v1/session", nil)
	r = r.WithContext(context.WithValue(r.Context(), peerKey{}, uint32(0)))
	w := httptest.NewRecorder()
	m.routes().ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatalf("root peer must still supply browser cookie: status=%d", w.Code)
	}
	if w := panelRequest(m, "POST", "/api/v1/login", `{"password":"test-password","remember_me":"yes"}`, nil, ""); w.Code != 400 {
		t.Fatalf("invalid remember_me status=%d", w.Code)
	}
	for i := 0; i < 1024; i++ {
		m.sessions[time.Unix(int64(i), 0).String()] = session{Expires: time.Now().Add(time.Hour)}
	}
	if w := panelRequest(m, "POST", "/api/v1/login", `{"password":"test-password"}`, nil, ""); w.Code != 503 {
		t.Fatalf("session capacity status=%d", w.Code)
	}
}

func TestPanelSessionPersistenceFailures(t *testing.T) {
	m := panelTestManager(t)
	if err := os.MkdirAll(sessionsPath(), 0700); err != nil {
		t.Fatal(err)
	}
	w := panelRequest(m, "POST", "/api/v1/login", `{"password":"test-password","remember_me":true}`, nil, "")
	if w.Code != 503 || len(w.Result().Cookies()) != 0 || len(m.sessions) != 0 {
		t.Fatalf("failed persistence issued a session: status=%d", w.Code)
	}
	if err := os.Remove(sessionsPath()); err != nil {
		t.Fatal(err)
	}
	cookie, csrf := panelLogin(t, m, true)
	if err := os.Remove(sessionsPath()); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(sessionsPath(), 0700); err != nil {
		t.Fatal(err)
	}
	w = panelRequest(m, "POST", "/api/v1/logout", "{}", cookie, csrf)
	if w.Code != 503 || len(w.Result().Cookies()) != 0 || len(m.sessions) != 1 {
		t.Fatalf("failed revocation must preserve session: status=%d", w.Code)
	}
	restarted := &manager{cfg: m.cfg, sessions: map[string]session{}}
	if err := restarted.loadSessions(); err == nil || len(restarted.sessions) != 0 {
		t.Fatal("unreadable sessions file was accepted")
	}
	if err := os.Remove(sessionsPath()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sessionsPath(), []byte("broken JSON"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := restarted.loadSessions(); err == nil || len(restarted.sessions) != 0 {
		t.Fatal("corrupt sessions file was accepted")
	}
}

func TestPasswordChangesRevokeRememberedSessions(t *testing.T) {
	for _, path := range []string{"/api/v1/password", "/api/v1/password/reset"} {
		t.Run(path, func(t *testing.T) {
			m := panelTestManager(t)
			h, err := openHistoryAt(dataDir)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(h.close)
			m.history = h
			cookie, csrf := panelLogin(t, m, true)
			method, body := "PUT", `{"password":"replacement-password"}`
			if strings.HasSuffix(path, "/reset") {
				method, body = "POST", "{}"
			}
			if w := panelRequest(m, method, path, body, cookie, csrf); w.Code != 200 {
				t.Fatalf("password update status=%d body=%s", w.Code, w.Body)
			}
			if w := panelRequest(m, "GET", "/api/v1/session", "", cookie, ""); w.Code != 401 {
				t.Fatalf("old session after password update status=%d", w.Code)
			}
			restarted := &manager{cfg: m.cfg, sessions: map[string]session{}}
			if err := restarted.loadSessions(); err != nil || len(restarted.sessions) != 0 {
				t.Fatalf("old credentials restored after restart: %v", err)
			}
		})
	}
}
