//go:build linux

package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestManagerRoutesRegisterWithoutConflict(t *testing.T) {
	m := &manager{}
	h := m.routes()

	root := httptest.NewRecorder()
	h.ServeHTTP(root, httptest.NewRequest(http.MethodGet, "/", nil))
	if root.Code != http.StatusOK {
		t.Fatalf("GET / status=%d", root.Code)
	}
	if !strings.Contains(root.Body.String(), "重置登录密码") {
		t.Fatal("panel password reset control missing")
	}

	api := httptest.NewRecorder()
	h.ServeHTTP(api, httptest.NewRequest(http.MethodGet, "/api/v1/status", nil))
	if api.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated API status=%d", api.Code)
	}
	connections := httptest.NewRecorder()
	h.ServeHTTP(connections, httptest.NewRequest(http.MethodGet, "/api/v1/connections?port=443", nil))
	if connections.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated connections status=%d", connections.Code)
	}
	reset := httptest.NewRecorder()
	h.ServeHTTP(reset, httptest.NewRequest(http.MethodPost, "/api/v1/password/reset", strings.NewReader("{}")))
	if reset.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated password reset status=%d", reset.Code)
	}
}

func TestSendBytesAndConnectionParsing(t *testing.T) {
	for _, tc := range []struct{ sent, retrans, expected uint64 }{{100, 0, 100}, {100, 20, 80}, {5, 10, 0}} {
		expected, actual := sendBytes(tc.sent, tc.retrans)
		if expected != tc.expected || actual != tc.sent {
			t.Fatalf("sendBytes(%d,%d)=%d,%d", tc.sent, tc.retrans, expected, actual)
		}
	}
	output := "ESTAB 0 0 192.0.2.1:443 198.51.100.4:50000 brutal_adaptive wscale:7,7\n" +
		"ESTAB 0 0 [2001:db8::1]:443 [2001:db8::2]:50001 cubic wscale:7,7\n" +
		"ESTAB 0 0 192.0.2.1:443 198.51.100.4:50002 brutal\n" +
		"TIME-WAIT 0 0 192.0.2.1:443 198.51.100.4:50003\n"
	rows, err := parseConnections([]byte(output), 443)
	if err != nil || len(rows) != 3 || !rows[0].Managed || rows[1].Managed || rows[2].Managed || rows[1].ClientIP != "2001:db8::2" || rows[2].ClientPort != 50002 {
		t.Fatalf("connections=%+v %v", rows, err)
	}
	rows, err = parseConnections(nil, 443)
	if err != nil || len(rows) != 0 {
		t.Fatalf("empty=%+v %v", rows, err)
	}
	if _, err = parseConnections([]byte("broken line"), 443); err == nil {
		t.Fatal("malformed ss output accepted")
	}
}

func TestAutostartState(t *testing.T) {
	query := func(manager, web string) func(string) (string, error) {
		return func(service string) (string, error) {
			if strings.Contains(service, "manager") {
				return manager, nil
			}
			return web, nil
		}
	}
	for _, tc := range []struct{ manager, web, want string }{{"enabled", "enabled", "on"}, {"disabled", "disabled", "off"}, {"enabled", "disabled", "partial"}} {
		got := autostartState(query(tc.manager, tc.web))["state"]
		if got != tc.want {
			t.Fatalf("%s/%s=%v", tc.manager, tc.web, got)
		}
	}
	unknown := autostartState(func(string) (string, error) { return "failed", errors.New("systemctl failed") })
	if unknown["state"] != "unknown" {
		t.Fatalf("unknown=%v", unknown)
	}
}
