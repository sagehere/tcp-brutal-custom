package main

import (
	"archive/zip"
	"bytes"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"io"
	"testing"
	"time"

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
	if err != nil || len(restarted) != 1 || restarted[0].Gap || restarted[0].Sent != 30 {
		t.Fatalf("restart checkpoint failed: %+v %v", restarted, err)
	}
}

func TestABHistoryEpochsAndReport(t *testing.T) {
	dir := t.TempDir()
	h, err := openHistoryAt(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer h.close()

	p := abPortConfig{Port: 443, RateMbps: 100, Gain: 20, CanaryPercent: 5, Enabled: true}
	epoch1, err := h.beginABEpoch(p, "test")
	if err != nil || epoch1 == 0 {
		t.Fatalf("begin epoch: %v %d", err, epoch1)
	}

	base0 := portState{Port: 443, Group: 11, Sent: 1000, Acked: 900, Retrans: 100, RTTSum: 10000, RTTSamples: 10, RTTMax: 2000, Members: 2}
	can0 := portState{Port: 443, Group: 22, Sent: 500, Acked: 470, Retrans: 30, RTTSum: 6000, RTTSamples: 6, RTTMax: 1800, Members: 1}
	if err = h.seedAB(443, base0, can0, selectorCount{Baseline: 90, Canary: 10}); err != nil {
		t.Fatal(err)
	}
	base1 := base0
	base1.Sent += 2000
	base1.Acked += 1900
	base1.Retrans += 100
	base1.RTTSum += 20000
	base1.RTTSamples += 20
	can1 := can0
	can1.Sent += 1000
	can1.Acked += 960
	can1.Retrans += 40
	can1.RTTSum += 9000
	can1.RTTSamples += 9
	if err = h.recordABCohort(epoch1, "baseline", base1, 110); err != nil {
		t.Fatal(err)
	}
	if err = h.recordABCohort(epoch1, "canary", can1, 110); err != nil {
		t.Fatal(err)
	}
	if err = h.recordABSelector(epoch1, 443, selectorCount{Baseline: 180, Canary: 20, Failure: 1}, 110); err != nil {
		t.Fatal(err)
	}
	if err = h.recordABApp(abAppSample{Time: 110, EpochID: epoch1, Port: 443, Cohort: "baseline", Source: "test", Requests: 90, Success: 89, Errors: 1, LatencySumUS: 90000, LatencySamples: 90, LatencyMaxUS: 4000}); err != nil {
		t.Fatal(err)
	}
	if err = h.recordABApp(abAppSample{Time: 110, EpochID: epoch1, Port: 443, Cohort: "canary", Source: "test", Requests: 10, Success: 10, Errors: 0, LatencySumUS: 8000, LatencySamples: 10, LatencyMaxUS: 1500}); err != nil {
		t.Fatal(err)
	}

	p.CanaryPercent = 50
	epoch2, err := h.beginABEpoch(p, "percentage_change")
	if err != nil || epoch2 == epoch1 {
		t.Fatalf("new epoch: %v %d", err, epoch2)
	}
	from := time.Now().Unix() - 3600
	to := time.Now().Unix() + 10
	epochs, err := h.abEpochs(443, from, to)
	if err != nil || len(epochs) != 2 || epochs[0].Ended == 0 || epochs[1].CanaryPercent != 50 {
		t.Fatalf("epochs=%+v err=%v", epochs, err)
	}
	ports, err := h.abPorts()
	if err != nil || len(ports) != 1 || ports[0] != 443 {
		t.Fatalf("A/B ports=%v err=%v", ports, err)
	}
	plans, err := h.abPlans(443, from, to)
	if err != nil || !plans[epoch1].Predeclared || plans[epoch1].Plan.BootstrapBlockMinutes != 5 {
		t.Fatalf("A/B epoch plan=%+v err=%v", plans[epoch1], err)
	}

	report, err := buildABReport(h, 443, from, to, "minute")
	if err != nil {
		t.Fatal(err)
	}
	zr, err := zip.NewReader(bytes.NewReader(report), int64(len(report)))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"manifest.json": false, "epochs.csv": false, "cohort_samples.csv": false, "selector_samples.csv": false, "summary.csv": false, "comparison.csv": false, "statistical_analysis.json": false, "analysis_plan.json": false, "analysis_rules.json": false}
	for _, zf := range zr.File {
		if _, ok := want[zf.Name]; ok {
			want[zf.Name] = true
		}
		if zf.Name == "manifest.json" {
			rc, e := zf.Open()
			if e != nil {
				t.Fatal(e)
			}
			b, e := io.ReadAll(rc)
			rc.Close()
			if e != nil {
				t.Fatal(e)
			}
			var m map[string]any
			if e = json.Unmarshal(b, &m); e != nil || int(m["schema_version"].(float64)) != 1 {
				t.Fatalf("manifest=%s err=%v", b, e)
			}
		}
	}
	for name, ok := range want {
		if !ok {
			t.Fatalf("report missing %s", name)
		}
	}
}

func TestABSchemaV1MigratesToV2(t *testing.T) {
	dir := t.TempDir()
	h, err := openHistoryAt(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = h.db.Exec("UPDATE ab_meta SET value='1' WHERE key='schema_version'"); err != nil {
		t.Fatal(err)
	}
	if _, err = h.db.Exec("DROP TABLE ab_epoch_plans"); err != nil {
		t.Fatal(err)
	}
	h.close()
	h, err = openHistoryAt(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer h.close()
	var version string
	if err = h.db.QueryRow("SELECT value FROM ab_meta WHERE key='schema_version'").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != "2" {
		t.Fatalf("schema version=%q", version)
	}
	if _, err = h.db.Exec("SELECT 1 FROM ab_epoch_plans LIMIT 1"); err != nil {
		t.Fatalf("plan table missing: %v", err)
	}
}

func TestABComparisonReadinessRejectsShortImbalancedEpoch(t *testing.T) {
	policy := defaultABAnalysisPolicy()
	rows := []abSummary{
		{EpochID: 9, Port: 443, Cohort: "baseline", CanaryPercent: 50, DurationSeconds: 60, AssignedConnections: 5, GoodputPerMemberMbps: 10},
		{EpochID: 9, Port: 443, Cohort: "canary", CanaryPercent: 50, DurationSeconds: 60, AssignedConnections: 15, GoodputPerMemberMbps: 9},
	}
	got := buildABComparisons(rows, policy)
	if len(got) != 1 {
		t.Fatalf("comparisons=%+v", got)
	}
	if got[0].NetworkReady || got[0].ApplicationReady {
		t.Fatalf("short imbalanced epoch unexpectedly ready: %+v", got[0])
	}
	if got[0].ActualCanaryPercent != 75 {
		t.Fatalf("actual canary percent=%v", got[0].ActualCanaryPercent)
	}
	if len(got[0].Reasons) == 0 {
		t.Fatal("missing readiness reasons")
	}
}

func TestABComparisonReadinessAcceptsHealthyEpoch(t *testing.T) {
	policy := defaultABAnalysisPolicy()
	rows := []abSummary{
		{EpochID: 10, Port: 443, Cohort: "baseline", CanaryPercent: 50, DurationSeconds: 3600, AssignedConnections: 1000, AppRequests: 5000, GapSamples: 0},
		{EpochID: 10, Port: 443, Cohort: "canary", CanaryPercent: 50, DurationSeconds: 3600, AssignedConnections: 1000, AppRequests: 5000, GapSamples: 0},
	}
	got := buildABComparisons(rows, policy)
	if len(got) != 1 || !got[0].NetworkReady || !got[0].ApplicationReady {
		t.Fatalf("healthy epoch not ready: %+v", got)
	}
}