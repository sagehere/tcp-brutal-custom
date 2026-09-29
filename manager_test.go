//go:build linux

package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
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
	if !strings.Contains(root.Body.String(), "A/B 实验") || !strings.Contains(root.Body.String(), "abAnalysis") {
		t.Fatal("A/B dashboard controls missing")
	}
	if !strings.Contains(root.Body.String(), "abDecision") || !strings.Contains(root.Body.String(), "abPlanNI") {
		t.Fatal("A/B statistical review controls missing")
	}
	if !strings.Contains(root.Body.String(), "abRollout") || !strings.Contains(root.Body.String(), "abRolloutStagesInput") || !strings.Contains(root.Body.String(), "abRolloutEvents") {
		t.Fatal("A/B rollout orchestration controls missing")
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

func TestFindPortStatePrefersActiveGroup(t *testing.T) {
	states := []portState{
		{Port: 5281, Group: 1, Active: false},
		{Port: 5281, Group: 2, Active: true},
	}
	got, ok := findPortState(states, 5281)
	if !ok || !got.Active || got.Group != 2 {
		t.Fatalf("state=%+v ok=%v", got, ok)
	}
}

func TestABCohortStatesAllowZeroShareToBeAbsent(t *testing.T) {
	canary := []portState{{Port: 5281, Group: 7, Active: true}}
	base, gotCanary, err := abCohortStates(abPortConfig{Port: 5281, CanaryPercent: 100}, nil, canary)
	if err != nil || base.Port != 5281 || base.Group != 0 || gotCanary.Group != 7 {
		t.Fatalf("100%% states base=%+v canary=%+v err=%v", base, gotCanary, err)
	}
	if _, _, err = abCohortStates(abPortConfig{Port: 5281, CanaryPercent: 50}, nil, canary); err == nil {
		t.Fatal("50% accepted a missing baseline cohort")
	}
}

func TestDeletePortIgnoresInactiveGroup(t *testing.T) {
	got := ""
	if err := deletePort(func(command string) error {
		got = command
		return unix.ENOENT
	}, 5281); err != nil {
		t.Fatalf("inactive delete returned %v", err)
	}
	if got != "del 5281" {
		t.Fatalf("delete command=%q", got)
	}
	want := errors.New("write failed")
	if err := deletePort(func(string) error { return want }, 5281); !errors.Is(err, want) {
		t.Fatalf("delete error=%v want=%v", err, want)
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