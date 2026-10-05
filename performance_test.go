//go:build linux

package main

import (
	"context"
	"database/sql"
	"encoding/csv"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSelectionFailuresAreNotSuccess(t *testing.T) {
	for _, c := range []selectorCount{{}, {Failure: 1}, {Success: 1, Failure: 1}} {
		if checkSelectorCount(c) == nil {
			t.Fatalf("accepted failed selection: %+v", c)
		}
	}
	if err := checkSelectorCount(selectorCount{Success: 1}); err != nil {
		t.Fatal(err)
	}
}

func TestPortAPICompatibilityAndBudgetReferences(t *testing.T) {
	oldConfigDir := configDir
	configDir = t.TempDir()
	t.Cleanup(func() { configDir = oldConfigDir })
	h, err := openHistoryAt(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer h.close()
	m := &manager{cfg: config{WebPort: 23333, Budgets: []budgetConfig{{Name: "wan", CapacityMbps: 100}}, Ports: []portConfig{{Port: 443, RateMbps: 10, Gain: 20, CompensationCapPercent: 110, Budget: "wan"}}}, history: h}
	post := func(body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		m.putPort(w, httptest.NewRequest(http.MethodPost, "/api/v1/ports", strings.NewReader(body)))
		return w
	}
	response := post(`{"port":443,"rate_mbps":15,"gain":20,"enabled":false}`)
	if response.Code != 200 {
		t.Fatalf("legacy request: %s", response.Body.String())
	}
	var port portConfig
	if err = json.Unmarshal(response.Body.Bytes(), &port); err != nil {
		t.Fatal(err)
	}
	if port.CompensationCapPercent != 110 || port.Budget != "wan" {
		t.Fatal("legacy request reset cap or budget")
	}
	for _, body := range []string{
		`{"port":443,"rate_mbps":15,"gain":20,"compensation_cap_percent":0}`,
		`{"port":443,"rate_mbps":15,"gain":20,"compensation_cap_percent":126}`,
		`{"port":443,"rate_mbps":15,"gain":20,"compensation_cap_percent":null}`,
		`{"port":443,"rate_mbps":15,"gain":20,"compensation_cap_percent":110.5}`,
		`{"port":443,"rate_mbps":15,"gain":20,"budget":"missing"}`,
		`{"port":443,"rate_mbps":15,"gain":20,"extra":true}`,
		`{"port":443,"rate_mbps":15,"gain":20} {}`,
	} {
		if w := post(body); w.Code != 400 {
			t.Fatalf("accepted invalid payload: %s (%d)", body, w.Code)
		}
	}
	if m.cfg.Ports[0].CompensationCapPercent != 110 {
		t.Fatal("failed update changed config")
	}
}

func TestDiagnosticsAndBudgetRequireAuthentication(t *testing.T) {
	handler := (&manager{}).routes()
	for _, path := range []string{"/api/v1/diagnose", "/api/v1/budgets"} {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		if w.Code != 401 {
			t.Fatalf("unprotected %s", path)
		}
	}
}

func TestBudgetAdvisoryAndValidation(t *testing.T) {
	c := config{Budgets: []budgetConfig{{Name: "wan", CapacityMbps: 100}}, Ports: []portConfig{{Port: 443, RateMbps: 80, Gain: 20, Enabled: true, Budget: "wan"}}}
	if err := validateBudgets(c.Budgets, c.Ports); err != nil {
		t.Fatal(err)
	}
	s := budgetStatuses(c)[0]
	if !s.Warning || s.RequestedMbps != 100 || s.ExcessMbps != 10 || math.Abs(s.ReduceTargetsPercent-10) > .000001 {
		t.Fatalf("wrong budget: %+v", s)
	}
	c.Ports[0].CompensationCapPercent = 100
	if budgetStatuses(c)[0].Warning {
		t.Fatal("disabled compensation not reflected")
	}
	for _, badRate := range []float64{math.NaN(), math.Inf(1), 0, 1000001} {
		c.Ports[0].RateMbps = badRate
		if validatePort(c.Ports[0], 23333) == nil {
			t.Fatal("accepted invalid port rate")
		}
	}
	c.Budgets[0].CapacityMbps = math.NaN()
	if validateBudgets(c.Budgets, c.Ports) == nil {
		t.Fatal("accepted nonfinite capacity")
	}
	if validateBudgets(nil, []portConfig{{Port: 1, Budget: "missing"}}) == nil {
		t.Fatal("accepted missing budget")
	}
}

func TestSamplingIntervalsAndConservation(t *testing.T) {
	h, err := openHistoryAt(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer h.close()
	record := func(sec int64, sent uint64) {
		t.Helper()
		if err := h.record(sample{Time: sec, Clock: time.Unix(sec, 0), Port: 443, Group: 1, Sent: sent, Acked: sent}); err != nil {
			t.Fatal(err)
		}
	}
	record(55, 0)
	record(65, 12500000)
	raw, err := h.query("raw", 60, 70, 443)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) != 1 || !raw[0].TimingValid || *raw[0].AckMbps != 10 {
		t.Fatalf("bad timed rate: %+v", raw)
	}
	minute, err := h.query("minute", 0, 120, 443)
	if err != nil {
		t.Fatal(err)
	}
	var total uint64
	for _, x := range minute {
		total += x.Sent
	}
	if total != 12500000 || len(minute) != 2 || minute[1].DurationMS != 5000 || !minute[1].TimingValid || *minute[1].AckMbps != 10 {
		t.Fatalf("bad overlap allocation: %+v", minute)
	}
	record(95, 50000000)
	gap, err := h.query("raw", 90, 100, 443)
	if err != nil {
		t.Fatal(err)
	}
	if len(gap) != 1 || !gap[0].Gap || gap[0].TimingValid || gap[0].AckMbps != nil {
		t.Fatalf("gap produced rate: %+v", gap)
	}
	record(105, 62500000)
	record(104, 63000000)
	back, err := h.query("raw", 100, 110, 443)
	if err != nil {
		t.Fatal(err)
	}
	if len(back) != 1 || !back[0].Gap || back[0].AckMbps != nil {
		t.Fatal("clock reversal produced rate")
	}
	// An ended group covers 31 minutes, another the full hour. Adding their
	// own averages would claim 14.68 Mbps rather than the actual 10 Mbps.
	for group, duration := range []int64{1860000, 3600000} {
		if _, err := h.db.Exec(`INSERT INTO samples VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, "hour", 3600, 443, group+1, 2250000000, 2250000000, 0, 0, 0, 0, 0, 0, 0, 0, 3600000, 3600000+duration, duration, 1); err != nil {
			t.Fatal(err)
		}
	}
	hour, err := h.query("hour", 3600, 7200, 443)
	if err != nil || len(hour) != 2 {
		t.Fatalf("partial hour query: %+v %v", hour, err)
	}
	for _, x := range hour {
		if !x.PartialCoverage || x.TimingValid || x.AckMbps != nil || x.SentMbps != nil || x.Acked != 2250000000 {
			t.Fatalf("unequal coverage fabricated aggregate rate: %+v", x)
		}
	}
}

func TestHistoryMigrationIsRepeatable(t *testing.T) {
	dir := t.TempDir()
	// A populated, pre-migration database, not a legacy row in a new schema.
	db, err := sql.Open("sqlite", filepath.Join(dir, "history.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`CREATE TABLE samples (tier TEXT NOT NULL,ts INTEGER NOT NULL,port INTEGER NOT NULL,group_id INTEGER NOT NULL,sent INTEGER NOT NULL,acked INTEGER NOT NULL,retrans INTEGER NOT NULL,success INTEGER NOT NULL,failure INTEGER NOT NULL,members INTEGER NOT NULL,rtt_sum INTEGER NOT NULL,rtt_samples INTEGER NOT NULL,rtt_max INTEGER NOT NULL,gap INTEGER NOT NULL,PRIMARY KEY(tier,ts,port,group_id))`); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO samples VALUES('raw',10,443,1,100,80,0,0,0,1,0,0,0,0)`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	h, err := openHistoryAt(dir)
	if err != nil {
		t.Fatal(err)
	}
	h.close()
	h, err = openHistoryAt(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer h.close()
	rows, err := h.query("raw", 0, 20, 443)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Sent != 100 || !rows[0].Legacy || rows[0].AckMbps != nil {
		t.Fatal("legacy counters or timing changed")
	}
}

func TestIsolatedBuildBlocksMaintenance(t *testing.T) {
	previous := algorithmName
	algorithmName = "brutal_review"
	defer func() { algorithmName = previous }()
	for _, path := range []string{"/api/v1/autostart", "/api/v1/update", "/api/v1/update/check"} {
		w := httptest.NewRecorder()
		(&manager{}).api(w, httptest.NewRequest(http.MethodPost, path, nil))
		if w.Code != 403 {
			t.Fatalf("isolated maintenance allowed: %s", path)
		}
	}
}

func TestDiagnosticBoundsAndMissingTools(t *testing.T) {
	output := &boundedOutput{limit: 4}
	if _, err := output.Write([]byte("12345")); err == nil || output.Len() > 4 {
		t.Fatal("diagnostic output bound not enforced")
	}
	if result := diagnosticCommand(context.Background(), "/nonexistent-review-diagnostic"); result.Available || result.Error == "" {
		t.Fatal("missing tool treated as available")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if result := diagnosticCommand(ctx, "sleep", "10"); result.Available || result.Error == "" {
		t.Fatal("cancelled command treated as available")
	}
}

func TestLegacyAndBackupConfigValidation(t *testing.T) {
	legacy := `{"web_host":"0.0.0.0","web_port":23333,"ports":[{"port":443,"rate_mbps":10,"gain":20}]}`
	c, err := parseConfig([]byte(legacy))
	if err != nil || c.Ports[0].CompensationCapPercent != 125 || len(c.Budgets) != 0 {
		t.Fatalf("legacy config upgrade: %+v %v", c, err)
	}
	for _, cap := range []string{"0", "99", "126", "null", "110.5"} {
		invalid := strings.Replace(legacy, `"gain":20`, `"gain":20,"compensation_cap_percent":`+cap, 1)
		if _, err := parseConfig([]byte(invalid)); err == nil {
			t.Fatalf("accepted cap %s in config/backup", cap)
		}
	}
	current := strings.Replace(legacy, `"gain":20`, `"gain":20,"compensation_cap_percent":110,"budget":"wan"`, 1)
	if _, err := parseConfig([]byte(strings.Replace(legacy, `"gain":20`, `"gain":20,"COMPENSATION_CAP_PERCENT":0`, 1))); err == nil {
		t.Fatal("case-insensitive config key bypassed cap validation")
	}
	current = strings.TrimSuffix(current, "}") + `,"budgets":[{"name":"wan","capacity_mbps":100}]}`
	if _, err := parseConfig([]byte(current)); err != nil {
		t.Fatal(err)
	}
}

func TestMetricExportKeepsUnknownRatesEmpty(t *testing.T) {
	h, err := openHistoryAt(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer h.close()
	for _, x := range []sample{{Time: 55, Port: 443, Group: 1}, {Time: 65, Port: 443, Group: 1, Acked: 12500000, Sent: 12500000}} {
		if err := h.record(x); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := h.db.Exec("INSERT INTO events VALUES(?,?,?)", 65, "marker", "{}"); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	(&manager{history: h}).metrics(w, httptest.NewRequest(http.MethodGet, "/api/v1/metrics?tier=raw&from=1&to=120&format=csv", nil))
	rows, err := csv.NewReader(strings.NewReader(w.Body.String())).ReadAll()
	if err != nil || w.Code != 200 || len(rows) != 4 {
		t.Fatalf("bad CSV export: %v %s", err, w.Body.String())
	}
	if len(rows[0]) != 24 || rows[0][23] != "partial_coverage" || rows[1][21] != "" || rows[1][22] != "" || rows[2][21] != "10.000000" || rows[2][23] != "false" || rows[3][13] != "marker:{}" {
		t.Fatal("CSV lost rates, gaps or event alignment")
	}
}
