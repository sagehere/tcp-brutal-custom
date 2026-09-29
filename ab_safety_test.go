package main

import (
	"testing"
	"time"
)

func containsAction(actions []string, want string) bool {
	for _, x := range actions {
		if x == want {
			return true
		}
	}
	return false
}

func safetyTestFixture(t *testing.T) (*history, *manager, abPortConfig, int64, int64) {
	t.Helper()
	h, err := openHistoryAt(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	analysis := defaultABExperimentPlan()
	safety := defaultABSafetyPlan()
	safety.WindowSeconds = 300
	safety.MinAssignedConnections = 10
	safety.MinAppRequestsPerCohort = 100
	safety.MaxSelectorFailurePercent = 1
	safety.MaxGapSamples = 0
	safety.MaxRetransDeltaPP = 1
	safety.MaxMeanRTTDeltaPercent = 20
	safety.MaxAppErrorDeltaPP = 2
	safety.MaxAppLatencyDeltaPercent = 30
	cfg := abPortConfig{
		Port: 11443, RateMbps: 100, Gain: 20, CanaryPercent: 50, Enabled: true,
		AnalysisPlan: &analysis, SafetyPlan: &safety,
	}
	epochID, err := h.beginABEpoch(cfg, "safety_test")
	if err != nil {
		h.close()
		t.Fatal(err)
	}
	started := time.Now().Unix() - 20
	started = started - started%10 + 5
	if _, err = h.db.Exec("UPDATE ab_epochs SET started_ts=? WHERE id=?", started, epochID); err != nil {
		h.close()
		t.Fatal(err)
	}
	// Deliberately store the first raw bucket before the exact epoch start.
	ts := started - started%10
	now := time.Now().Unix()
	_, err = h.db.Exec("INSERT INTO ab_samples(tier,ts,epoch_id,port,cohort,group_id,duration_sec,member_seconds,sent,acked,retrans,members,rtt_sum,rtt_samples,rtt_max,gap) VALUES"+
		"('raw',?,?,?,?,1,10,10,1000000,990000,10000,10,1000000,100,12000,0),"+
		"('raw',?,?,?,?,2,10,10,1000000,960000,40000,10,2000000,100,24000,0)",
		ts, epochID, cfg.Port, "baseline", ts, epochID, cfg.Port, "canary")
	if err != nil {
		h.close()
		t.Fatal(err)
	}
	_, err = h.db.Exec("INSERT INTO ab_selector_samples(tier,ts,epoch_id,port,baseline,canary,failure,gap) VALUES('raw',?,?,?,?,?,?,0)", ts, epochID, cfg.Port, 50, 50, 0)
	if err != nil {
		h.close()
		t.Fatal(err)
	}
	_, err = h.db.Exec("INSERT INTO ab_app_samples(tier,ts,epoch_id,port,cohort,source,requests,success,errors,latency_sum_us,latency_samples,latency_max_us) VALUES"+
		"('raw',?,?,?,?,?,1000,990,10,1000000,1000,2000),"+
		"('raw',?,?,?,?,?,1000,900,100,3000000,1000,5000)",
		ts, epochID, cfg.Port, "baseline", "test", ts, epochID, cfg.Port, "canary", "test")
	if err != nil {
		h.close()
		t.Fatal(err)
	}
	m := &manager{cfg: config{ABPorts: []abPortConfig{cfg}}, history: h}
	return h, m, cfg, epochID, now
}

func TestABSafetyCriticalAlertLifecycle(t *testing.T) {
	h, m, cfg, epochID, now := safetyTestFixture(t)
	defer h.close()
	eval, err := m.evaluateABSafety(cfg, epochID, now)
	if err != nil {
		t.Fatal(err)
	}
	if eval.Status != "critical" || !eval.HardBlock || !eval.RollbackRecommended {
		t.Fatalf("critical evaluation=%+v", eval)
	}
	if len(eval.Breaches) < 3 {
		t.Fatalf("expected multiple breaches, got %+v", eval.Breaches)
	}
	if err = h.syncABSafetyAlerts(eval, 7); err != nil {
		t.Fatal(err)
	}
	active, err := h.activeABSafetyAlerts(cfg.Port)
	if err != nil || len(active) != len(eval.Breaches) {
		t.Fatalf("active alerts=%+v err=%v", active, err)
	}
	for _, a := range active {
		if !a.Active || a.RolloutID != 7 || a.EpochID != epochID {
			t.Fatalf("invalid active alert=%+v", a)
		}
	}

	if _, err = h.db.Exec("UPDATE ab_samples SET retrans=5000,rtt_sum=900000 WHERE epoch_id=? AND cohort='canary' AND tier='raw'", epochID); err != nil {
		t.Fatal(err)
	}
	if _, err = h.db.Exec("UPDATE ab_app_samples SET errors=5,success=995,latency_sum_us=900000 WHERE epoch_id=? AND cohort='canary' AND tier='raw'", epochID); err != nil {
		t.Fatal(err)
	}
	eval, err = m.evaluateABSafety(cfg, epochID, now)
	if err != nil {
		t.Fatal(err)
	}
	if eval.Status != "clear" || eval.HardBlock || eval.RollbackRecommended {
		t.Fatalf("cleared evaluation=%+v", eval)
	}
	if err = h.syncABSafetyAlerts(eval, 7); err != nil {
		t.Fatal(err)
	}
	active, err = h.activeABSafetyAlerts(cfg.Port)
	if err != nil || len(active) != 0 {
		t.Fatalf("alerts did not clear: %+v err=%v", active, err)
	}
	var cleared int
	if err = h.db.QueryRow("SELECT count(*) FROM ab_safety_alerts WHERE port=? AND active=0 AND cleared_ts IS NOT NULL", cfg.Port).Scan(&cleared); err != nil {
		t.Fatal(err)
	}
	if cleared < 3 {
		t.Fatalf("cleared alerts=%d", cleared)
	}
}

func TestABSafetyGateBlocksAdvanceAndRetryOnCritical(t *testing.T) {
	h, m, cfg, _, _ := safetyTestFixture(t)
	defer h.close()
	view := &abRolloutView{
		WindowState:    "eligible_review",
		AllowedActions: []string{"advance", "retry", "pause", "rollback"},
	}
	got, err := m.finalizeRolloutView(cfg.Port, view)
	if err != nil {
		t.Fatal(err)
	}
	if !got.SafetyBlocked || !got.RollbackRecommended || got.SafetyStatus != "critical" {
		t.Fatalf("safety gate=%+v", got)
	}
	if containsAction(got.AllowedActions, "advance") || containsAction(got.AllowedActions, "retry") {
		t.Fatalf("unsafe actions remained: %v", got.AllowedActions)
	}
	if !containsAction(got.AllowedActions, "pause") || !containsAction(got.AllowedActions, "rollback") {
		t.Fatalf("safe actions missing: %v", got.AllowedActions)
	}
}

func TestABSafetyWarmingUpIsFailClosedWithoutAlert(t *testing.T) {
	h, m, cfg, epochID, now := safetyTestFixture(t)
	defer h.close()
	cfg.SafetyPlan.MinAssignedConnections = 1000
	m.cfg.ABPorts[0] = cfg
	eval, err := m.evaluateABSafety(cfg, epochID, now)
	if err != nil {
		t.Fatal(err)
	}
	if eval.Status != "warming_up" || !eval.HardBlock || eval.RollbackRecommended || len(eval.Breaches) != 0 {
		t.Fatalf("warming evaluation=%+v", eval)
	}
	if err = h.syncABSafetyAlerts(eval, 0); err != nil {
		t.Fatal(err)
	}
	active, err := h.activeABSafetyAlerts(cfg.Port)
	if err != nil || len(active) != 0 {
		t.Fatalf("warming created alerts=%+v err=%v", active, err)
	}
}

func TestABSafetyPlanStoredWithEpoch(t *testing.T) {
	h, _, cfg, epochID, _ := safetyTestFixture(t)
	defer h.close()
	var window, minConn, minApp, gaps uint64
	var selector, retrans, rtt, appErr, appLatency float64
	var predeclared int
	err := h.db.QueryRow("SELECT window_seconds,min_assigned_connections,min_app_requests_per_cohort,max_selector_failure_percent,max_gap_samples,max_retrans_delta_pp,max_mean_rtt_delta_percent,max_app_error_delta_pp,max_app_latency_delta_percent,predeclared FROM ab_epoch_safety_plans WHERE epoch_id=?", epochID).
		Scan(&window, &minConn, &minApp, &selector, &gaps, &retrans, &rtt, &appErr, &appLatency, &predeclared)
	if err != nil {
		t.Fatal(err)
	}
	if window != cfg.SafetyPlan.WindowSeconds || minConn != cfg.SafetyPlan.MinAssignedConnections || predeclared != 1 {
		t.Fatalf("stored safety plan mismatch window=%d min=%d predeclared=%d", window, minConn, predeclared)
	}
}