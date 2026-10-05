package main

import (
	"crypto/subtle"
	"encoding/hex"
	"testing"

	"golang.org/x/crypto/argon2"
)

func TestPasswordHashMatchesLogin(t *testing.T) {
	var c config
	if err := setPassword(&c, "long-test-password-123"); err != nil {
		t.Fatal(err)
	}
	salt, _ := hex.DecodeString(c.PasswordSalt)
	want, _ := hex.DecodeString(c.PasswordHash)
	got := argon2.IDKey([]byte("long-test-password-123"), salt, 3, 64*1024, 4, 32)
	if subtle.ConstantTimeCompare(got, want) != 1 {
		t.Fatal("saved password cannot authenticate")
	}
}

func TestPasswordMinimumUnicodeCharacters(t *testing.T) {
	for _, password := range []string{"1234567", "中文中文中文中", "🦊🦊🦊🦊🦊🦊🦊"} {
		var c config
		if setPassword(&c, password) == nil {
			t.Fatalf("accepted short password %q", password)
		}
	}
	for _, password := range []string{"12345678", "中文中文中文中文文", "🦊🦊🦊🦊🦊🦊🦊🦊"} {
		var c config
		if err := setPassword(&c, password); err != nil {
			t.Fatalf("rejected %q: %v", password, err)
		}
	}
}

func TestHistoryDeltasAndReset(t *testing.T) {
	dir := t.TempDir()
	h, err := openHistoryAt(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { h.close() }()
	rows := []sample{
		{Time: 100, Port: 443, Group: 1, Sent: 100, Acked: 80, Retrans: 10, RTTSum: 1000, RTTSamples: 1},
		{Time: 110, Port: 443, Group: 1, Sent: 300, Acked: 240, Retrans: 30, RTTSum: 2500, RTTSamples: 2},
		{Time: 120, Port: 443, Group: 1, Sent: 20, Acked: 15, Retrans: 1, RTTSum: 30, RTTSamples: 1},
	}
	for _, x := range rows {
		if err := h.record(x); err != nil {
			t.Fatal(err)
		}
	}
	got, err := h.query("raw", 100, 130, 443)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || !got[0].Gap || got[0].Sent != 100 || got[1].Gap || got[1].Sent != 200 || got[1].Retrans != 20 || !got[2].Gap || got[2].Sent != 0 {
		t.Fatalf("unexpected deltas: %+v", got)
	}
	minute, err := h.query("minute", 60, 180, 443)
	if err != nil {
		t.Fatal(err)
	}
	if len(minute) != 2 || minute[0].Sent != 300 || minute[0].Retrans != 30 {
		t.Fatalf("bad rollup: %+v", minute)
	}
	h.close()
	h, err = openHistoryAt(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err = h.record(sample{Time: 130, Port: 443, Group: 1, Sent: 50, Acked: 35, Retrans: 2, RTTSum: 50, RTTSamples: 2}); err != nil {
		t.Fatal(err)
	}
	restarted, err := h.query("raw", 130, 140, 443)
	if err != nil || len(restarted) != 1 || !restarted[0].Gap || restarted[0].TimingValid || restarted[0].Sent != 30 {
		t.Fatalf("restart checkpoint failed: %+v %v", restarted, err)
	}
}
