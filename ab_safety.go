package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

type abSafetyPlan struct {
	WindowSeconds             uint64  `json:"window_seconds"`
	MinAssignedConnections    uint64  `json:"min_assigned_connections"`
	MinAppRequestsPerCohort   uint64  `json:"min_app_requests_per_cohort"`
	MaxSelectorFailurePercent float64 `json:"max_selector_failure_percent"`
	MaxGapSamples             uint64  `json:"max_gap_samples"`
	MaxRetransDeltaPP         float64 `json:"max_retrans_delta_pp"`
	MaxMeanRTTDeltaPercent    float64 `json:"max_mean_rtt_delta_percent"`
	MaxAppErrorDeltaPP        float64 `json:"max_app_error_delta_pp"`
	MaxAppLatencyDeltaPercent float64 `json:"max_app_latency_delta_percent"`
}

func defaultABSafetyPlan() abSafetyPlan {
	return abSafetyPlan{
		WindowSeconds: 300, MinAssignedConnections: 50, MinAppRequestsPerCohort: 100,
		MaxSelectorFailurePercent: 1.0, MaxGapSamples: 0, MaxRetransDeltaPP: 2.0,
		MaxMeanRTTDeltaPercent: 50.0, MaxAppErrorDeltaPP: 2.0, MaxAppLatencyDeltaPercent: 50.0,
	}
}

func effectiveABSafetyPlan(in *abSafetyPlan) (abSafetyPlan, error) {
	if in == nil {
		return defaultABSafetyPlan(), nil
	}
	p := *in
	switch {
	case p.WindowSeconds < 30 || p.WindowSeconds > 3600:
		return p, fmt.Errorf("safety window must be between 30 and 3600 seconds")
	case p.MinAssignedConnections < 1 || p.MinAssignedConnections > 1000000000:
		return p, fmt.Errorf("minimum assigned connections must be between 1 and 1000000000")
	case p.MinAppRequestsPerCohort > 1000000000:
		return p, fmt.Errorf("minimum application requests per cohort is too large")
	case p.MaxSelectorFailurePercent < 0 || p.MaxSelectorFailurePercent > 100:
		return p, fmt.Errorf("maximum selector failure percent must be in [0,100]")
	case p.MaxGapSamples > 1000000:
		return p, fmt.Errorf("maximum gap samples is too large")
	case p.MaxRetransDeltaPP < 0 || p.MaxRetransDeltaPP > 100:
		return p, fmt.Errorf("maximum retransmission delta must be in [0,100] percentage points")
	case p.MaxMeanRTTDeltaPercent < 0 || p.MaxMeanRTTDeltaPercent > 5000:
		return p, fmt.Errorf("maximum RTT delta must be in [0,5000] percent")
	case p.MaxAppErrorDeltaPP < 0 || p.MaxAppErrorDeltaPP > 100:
		return p, fmt.Errorf("maximum application error delta must be in [0,100] percentage points")
	case p.MaxAppLatencyDeltaPercent < 0 || p.MaxAppLatencyDeltaPercent > 5000:
		return p, fmt.Errorf("maximum application latency delta must be in [0,5000] percent")
	}
	return p, nil
}

type abStoredSafetyPlan struct {
	Plan        abSafetyPlan
	Predeclared bool
}

type abSafetyMetrics struct {
	AssignedConnections     uint64  `json:"assigned_connections"`
	SelectorFailures        uint64  `json:"selector_failures"`
	SelectorFailurePercent  float64 `json:"selector_failure_percent"`
	GapSamples              uint64  `json:"gap_samples"`
	BaselineRetransPercent  float64 `json:"baseline_retrans_percent"`
	CanaryRetransPercent    float64 `json:"canary_retrans_percent"`
	RetransDeltaPP          float64 `json:"retrans_delta_pp"`
	BaselineMeanRTTMS       float64 `json:"baseline_mean_rtt_ms"`
	CanaryMeanRTTMS         float64 `json:"canary_mean_rtt_ms"`
	MeanRTTDeltaPercent     float64 `json:"mean_rtt_delta_percent"`
	BaselineAppRequests     uint64  `json:"baseline_app_requests"`
	CanaryAppRequests       uint64  `json:"canary_app_requests"`
	BaselineAppErrorPercent float64 `json:"baseline_app_error_percent"`
	CanaryAppErrorPercent   float64 `json:"canary_app_error_percent"`
	AppErrorDeltaPP         float64 `json:"app_error_delta_pp"`
	BaselineAppLatencyMS    float64 `json:"baseline_app_latency_ms"`
	CanaryAppLatencyMS      float64 `json:"canary_app_latency_ms"`
	AppLatencyDeltaPercent  float64 `json:"app_latency_delta_percent"`
	NetworkComparable       bool    `json:"network_comparable"`
	ApplicationComparable   bool    `json:"application_comparable"`
}

type abSafetyBreach struct {
	Code                string `json:"code"`
	Severity            string `json:"severity"`
	Message             string `json:"message"`
	RollbackRecommended bool   `json:"rollback_recommended"`
}

type abSafetyEvaluation struct {
	Port                uint16           `json:"port"`
	EpochID             int64            `json:"epoch_id"`
	CanaryPercent       uint8            `json:"canary_percent"`
	From                int64            `json:"from"`
	To                  int64            `json:"to"`
	Status              string           `json:"status"`
	HardBlock           bool             `json:"hard_block"`
	RollbackRecommended bool             `json:"rollback_recommended"`
	Plan                abSafetyPlan     `json:"plan"`
	Metrics             abSafetyMetrics  `json:"metrics"`
	Breaches            []abSafetyBreach `json:"breaches,omitempty"`
	Reasons             []string         `json:"reasons,omitempty"`
}

type abSafetyAlert struct {
	ID         int64  `json:"id"`
	Port       uint16 `json:"port"`
	EpochID    int64  `json:"epoch_id"`
	RolloutID  int64  `json:"rollout_id,omitempty"`
	FirstSeen  int64  `json:"first_seen"`
	LastSeen   int64  `json:"last_seen"`
	Cleared    int64  `json:"cleared,omitempty"`
	Severity   string `json:"severity"`
	Code       string `json:"code"`
	Message    string `json:"message"`
	DetailJSON string `json:"detail_json,omitempty"`
	Active     bool   `json:"active"`
}

type abSafetyView struct {
	Evaluation   abSafetyEvaluation `json:"evaluation"`
	Alerts       []abSafetyAlert    `json:"alerts"`
	RecentAlerts []abSafetyAlert    `json:"recent_alerts,omitempty"`
}

type safetyNetAgg struct{ sent, retrans, rttSum, rttSamples, gaps uint64 }
type safetyAppAgg struct{ requests, errors, latencySumUS, latencySamples uint64 }

func (m *manager) evaluateABSafety(cfg abPortConfig, epochID, now int64) (abSafetyEvaluation, error) {
	plan, err := effectiveABSafetyPlan(cfg.SafetyPlan)
	if err != nil {
		return abSafetyEvaluation{}, err
	}
	out := abSafetyEvaluation{Port: cfg.Port, EpochID: epochID, CanaryPercent: cfg.CanaryPercent, To: now, Plan: plan}
	if cfg.CanaryPercent == 0 || cfg.CanaryPercent == 100 {
		out.From = now - int64(plan.WindowSeconds)
		out.Status = "not_comparable"
		out.Reasons = []string{"live safety comparison requires simultaneous baseline and canary cohorts"}
		return out, nil
	}
	var epochStarted int64
	if err := m.history.db.QueryRow("SELECT started_ts FROM ab_epochs WHERE id=?", epochID).Scan(&epochStarted); err != nil {
		return out, err
	}
	out.From = now - int64(plan.WindowSeconds)
	if out.From < epochStarted {
		out.From = epochStarted
	}

	// Raw samples are stored in 10-second buckets. Query the bucket that overlaps
	// the exact window/epoch boundary, then keep the epoch filter below.
	bucketFrom := out.From - out.From%10
	samples, err := m.history.abSamples("raw", cfg.Port, bucketFrom, now+1)
	if err != nil {
		return out, err
	}
	selectors, err := m.history.abSelectorSamples("raw", cfg.Port, bucketFrom, now+1)
	if err != nil {
		return out, err
	}
	app, err := m.history.abAppSamples("raw", cfg.Port, bucketFrom, now+1)
	if err != nil {
		return out, err
	}

	var base, can safetyNetAgg
	for _, x := range samples {
		if x.EpochID != epochID {
			continue
		}
		var a *safetyNetAgg
		if x.Cohort == "baseline" {
			a = &base
		} else if x.Cohort == "canary" {
			a = &can
		} else {
			continue
		}
		a.sent += x.Sent
		a.retrans += x.Retrans
		a.rttSum += x.RTTSum
		a.rttSamples += x.RTTSamples
		if x.Gap {
			a.gaps++
		}
	}
	var assigned, failures, selectorGaps uint64
	for _, x := range selectors {
		if x.EpochID != epochID {
			continue
		}
		assigned += x.Baseline + x.Canary
		failures += x.Failure
		if x.Gap {
			selectorGaps++
		}
	}
	var baseApp, canApp safetyAppAgg
	for _, x := range app {
		if x.EpochID != epochID {
			continue
		}
		var a *safetyAppAgg
		if x.Cohort == "baseline" {
			a = &baseApp
		} else if x.Cohort == "canary" {
			a = &canApp
		} else {
			continue
		}
		a.requests += x.Requests
		a.errors += x.Errors
		a.latencySumUS += x.LatencySumUS
		a.latencySamples += x.LatencySamples
	}

	metrics := abSafetyMetrics{
		AssignedConnections: assigned, SelectorFailures: failures, GapSamples: base.gaps + can.gaps + selectorGaps,
		BaselineAppRequests: baseApp.requests, CanaryAppRequests: canApp.requests,
	}
	if assigned+failures > 0 {
		metrics.SelectorFailurePercent = 100 * float64(failures) / float64(assigned+failures)
	}
	if base.sent > 0 {
		metrics.BaselineRetransPercent = 100 * float64(base.retrans) / float64(base.sent)
	}
	if can.sent > 0 {
		metrics.CanaryRetransPercent = 100 * float64(can.retrans) / float64(can.sent)
	}
	if base.sent > 0 && can.sent > 0 {
		metrics.RetransDeltaPP = metrics.CanaryRetransPercent - metrics.BaselineRetransPercent
		metrics.NetworkComparable = true
	}
	if base.rttSamples > 0 {
		metrics.BaselineMeanRTTMS = float64(base.rttSum) / float64(base.rttSamples) / 1000
	}
	if can.rttSamples > 0 {
		metrics.CanaryMeanRTTMS = float64(can.rttSum) / float64(can.rttSamples) / 1000
	}
	if metrics.BaselineMeanRTTMS > 0 && metrics.CanaryMeanRTTMS > 0 {
		metrics.MeanRTTDeltaPercent = 100 * (metrics.CanaryMeanRTTMS - metrics.BaselineMeanRTTMS) / metrics.BaselineMeanRTTMS
	}
	if baseApp.requests > 0 {
		metrics.BaselineAppErrorPercent = 100 * float64(baseApp.errors) / float64(baseApp.requests)
	}
	if canApp.requests > 0 {
		metrics.CanaryAppErrorPercent = 100 * float64(canApp.errors) / float64(canApp.requests)
	}
	if baseApp.latencySamples > 0 {
		metrics.BaselineAppLatencyMS = float64(baseApp.latencySumUS) / float64(baseApp.latencySamples) / 1000
	}
	if canApp.latencySamples > 0 {
		metrics.CanaryAppLatencyMS = float64(canApp.latencySumUS) / float64(canApp.latencySamples) / 1000
	}
	if baseApp.requests >= plan.MinAppRequestsPerCohort && canApp.requests >= plan.MinAppRequestsPerCohort {
		metrics.ApplicationComparable = true
		metrics.AppErrorDeltaPP = metrics.CanaryAppErrorPercent - metrics.BaselineAppErrorPercent
		if metrics.BaselineAppLatencyMS > 0 && metrics.CanaryAppLatencyMS > 0 {
			metrics.AppLatencyDeltaPercent = 100 * (metrics.CanaryAppLatencyMS - metrics.BaselineAppLatencyMS) / metrics.BaselineAppLatencyMS
		}
	}
	out.Metrics = metrics

	if assigned < plan.MinAssignedConnections {
		out.Status = "warming_up"
		out.HardBlock = true
		out.Reasons = append(out.Reasons, fmt.Sprintf("recent safety window connections %d < %d", assigned, plan.MinAssignedConnections))
		return out, nil
	}
	add := func(code, severity, message string, rollback bool) {
		out.Breaches = append(out.Breaches, abSafetyBreach{Code: code, Severity: severity, Message: message, RollbackRecommended: rollback})
		if rollback {
			out.RollbackRecommended = true
		}
	}
	if metrics.SelectorFailurePercent > plan.MaxSelectorFailurePercent {
		add("selector_failure", "critical", fmt.Sprintf("selector failure %.3f%% > %.3f%%", metrics.SelectorFailurePercent, plan.MaxSelectorFailurePercent), true)
	}
	if metrics.GapSamples > plan.MaxGapSamples {
		add("data_gap", "warning", fmt.Sprintf("recent gap samples %d > %d", metrics.GapSamples, plan.MaxGapSamples), false)
	}
	if metrics.NetworkComparable && metrics.RetransDeltaPP > plan.MaxRetransDeltaPP {
		add("retrans_regression", "critical", fmt.Sprintf("retransmission delta %.3fpp > %.3fpp", metrics.RetransDeltaPP, plan.MaxRetransDeltaPP), true)
	}
	if metrics.NetworkComparable && metrics.BaselineMeanRTTMS > 0 && metrics.CanaryMeanRTTMS > 0 && metrics.MeanRTTDeltaPercent > plan.MaxMeanRTTDeltaPercent {
		add("rtt_regression", "critical", fmt.Sprintf("mean RTT delta %.2f%% > %.2f%%", metrics.MeanRTTDeltaPercent, plan.MaxMeanRTTDeltaPercent), true)
	}
	if metrics.ApplicationComparable && metrics.AppErrorDeltaPP > plan.MaxAppErrorDeltaPP {
		add("app_error_regression", "critical", fmt.Sprintf("application error delta %.3fpp > %.3fpp", metrics.AppErrorDeltaPP, plan.MaxAppErrorDeltaPP), true)
	}
	if metrics.ApplicationComparable && metrics.BaselineAppLatencyMS > 0 && metrics.CanaryAppLatencyMS > 0 && metrics.AppLatencyDeltaPercent > plan.MaxAppLatencyDeltaPercent {
		add("app_latency_regression", "critical", fmt.Sprintf("application latency delta %.2f%% > %.2f%%", metrics.AppLatencyDeltaPercent, plan.MaxAppLatencyDeltaPercent), true)
	}
	if len(out.Breaches) == 0 {
		out.Status = "clear"
		return out, nil
	}
	out.HardBlock = true
	out.Status = "warning"
	for _, b := range out.Breaches {
		if b.Severity == "critical" {
			out.Status = "critical"
			break
		}
	}
	return out, nil
}

func (h *history) abSafetyPlans(port uint16, from, to int64) (map[int64]abStoredSafetyPlan, error) {
	rows, err := h.db.Query("SELECT p.epoch_id,p.window_seconds,p.min_assigned_connections,p.min_app_requests_per_cohort,p.max_selector_failure_percent,p.max_gap_samples,p.max_retrans_delta_pp,p.max_mean_rtt_delta_percent,p.max_app_error_delta_pp,p.max_app_latency_delta_percent,p.predeclared FROM ab_epoch_safety_plans p JOIN ab_epochs e ON e.id=p.epoch_id WHERE e.port=? AND e.started_ts<? AND COALESCE(e.ended_ts,?)>=?", port, to, to, from)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]abStoredSafetyPlan{}
	for rows.Next() {
		var id int64
		var p abSafetyPlan
		var pre int
		if err := rows.Scan(&id, &p.WindowSeconds, &p.MinAssignedConnections, &p.MinAppRequestsPerCohort, &p.MaxSelectorFailurePercent, &p.MaxGapSamples, &p.MaxRetransDeltaPP, &p.MaxMeanRTTDeltaPercent, &p.MaxAppErrorDeltaPP, &p.MaxAppLatencyDeltaPercent, &pre); err != nil {
			return nil, err
		}
		out[id] = abStoredSafetyPlan{Plan: p, Predeclared: pre != 0}
	}
	return out, rows.Err()
}

func (h *history) activeABSafetyAlerts(port uint16) ([]abSafetyAlert, error) {
	rows, err := h.db.Query("SELECT id,port,epoch_id,COALESCE(rollout_id,0),first_seen_ts,last_seen_ts,COALESCE(cleared_ts,0),severity,code,message,COALESCE(detail_json,''),active FROM ab_safety_alerts WHERE port=? AND active=1 ORDER BY severity DESC,first_seen_ts", port)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []abSafetyAlert
	for rows.Next() {
		var x abSafetyAlert
		if err := rows.Scan(&x.ID, &x.Port, &x.EpochID, &x.RolloutID, &x.FirstSeen, &x.LastSeen, &x.Cleared, &x.Severity, &x.Code, &x.Message, &x.DetailJSON, &x.Active); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

func (h *history) abSafetyAlerts(port uint16, from, to int64) ([]abSafetyAlert, error) {
	rows, err := h.db.Query("SELECT id,port,epoch_id,COALESCE(rollout_id,0),first_seen_ts,last_seen_ts,COALESCE(cleared_ts,0),severity,code,message,COALESCE(detail_json,''),active FROM ab_safety_alerts WHERE port=? AND first_seen_ts<? AND COALESCE(cleared_ts,?)>=? ORDER BY id", port, to, to, from)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []abSafetyAlert
	for rows.Next() {
		var x abSafetyAlert
		if err := rows.Scan(&x.ID, &x.Port, &x.EpochID, &x.RolloutID, &x.FirstSeen, &x.LastSeen, &x.Cleared, &x.Severity, &x.Code, &x.Message, &x.DetailJSON, &x.Active); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

func (h *history) syncABSafetyAlerts(eval abSafetyEvaluation, rolloutID int64) error {
	now := eval.To
	active, err := h.activeABSafetyAlerts(eval.Port)
	if err != nil {
		return err
	}
	byCode := map[string]abSafetyAlert{}
	for _, x := range active {
		byCode[x.Code] = x
	}
	current := map[string]abSafetyBreach{}
	for _, b := range eval.Breaches {
		current[b.Code] = b
		detail, _ := json.Marshal(map[string]any{"evaluation": eval, "breach": b})
		if old, ok := byCode[b.Code]; ok {
			_, err = h.db.Exec("UPDATE ab_safety_alerts SET epoch_id=?,rollout_id=?,last_seen_ts=?,severity=?,message=?,detail_json=? WHERE id=?", eval.EpochID, nullableRolloutID(rolloutID), now, b.Severity, b.Message, string(detail), old.ID)
		} else {
			_, err = h.db.Exec("INSERT INTO ab_safety_alerts(port,epoch_id,rollout_id,first_seen_ts,last_seen_ts,severity,code,message,detail_json,active) VALUES(?,?,?,?,?,?,?,?,?,1)", eval.Port, eval.EpochID, nullableRolloutID(rolloutID), now, now, b.Severity, b.Code, b.Message, string(detail))
			if err == nil {
				h.addEvent("ab_safety_alert", map[string]any{"port": eval.Port, "epoch_id": eval.EpochID, "code": b.Code, "severity": b.Severity, "message": b.Message})
			}
		}
		if err != nil {
			return err
		}
	}
	if eval.Status == "warming_up" {
		return nil
	}
	for _, old := range active {
		if _, ok := current[old.Code]; ok {
			continue
		}
		if _, err := h.db.Exec("UPDATE ab_safety_alerts SET active=0,cleared_ts=?,last_seen_ts=? WHERE id=?", now, now, old.ID); err != nil {
			return err
		}
		h.addEvent("ab_safety_clear", map[string]any{"port": eval.Port, "epoch_id": eval.EpochID, "code": old.Code})
	}
	return nil
}

func nullableRolloutID(id int64) any {
	if id == 0 {
		return nil
	}
	return id
}

func (h *history) currentABSafetyConfig(port uint16) (*abPortConfig, int64, error) {
	var epochID int64
	var cfg abPortConfig
	var p abSafetyPlan
	err := h.db.QueryRow("SELECT e.id,e.port,e.canary_percent,p.window_seconds,p.min_assigned_connections,p.min_app_requests_per_cohort,p.max_selector_failure_percent,p.max_gap_samples,p.max_retrans_delta_pp,p.max_mean_rtt_delta_percent,p.max_app_error_delta_pp,p.max_app_latency_delta_percent FROM ab_epochs e JOIN ab_epoch_safety_plans p ON p.epoch_id=e.id WHERE e.port=? AND e.ended_ts IS NULL ORDER BY e.id DESC LIMIT 1", port).
		Scan(&epochID, &cfg.Port, &cfg.CanaryPercent, &p.WindowSeconds, &p.MinAssignedConnections, &p.MinAppRequestsPerCohort, &p.MaxSelectorFailurePercent, &p.MaxGapSamples, &p.MaxRetransDeltaPP, &p.MaxMeanRTTDeltaPercent, &p.MaxAppErrorDeltaPP, &p.MaxAppLatencyDeltaPercent)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, 0, nil
		}
		return nil, 0, err
	}
	cfg.SafetyPlan = &p
	return &cfg, epochID, nil
}

func (m *manager) safetyView(port uint16) (*abSafetyView, error) {
	cfg, epochID, err := m.history.currentABSafetyConfig(port)
	if err != nil {
		return nil, err
	}
	if cfg == nil {
		return nil, nil
	}
	eval, err := m.evaluateABSafety(*cfg, epochID, time.Now().Unix())
	if err != nil {
		return nil, err
	}
	alerts, err := m.history.activeABSafetyAlerts(port)
	if err != nil {
		return nil, err
	}
	recent, err := m.history.abSafetyAlerts(port, time.Now().Unix()-24*3600, time.Now().Unix()+1)
	if err != nil {
		return nil, err
	}
	return &abSafetyView{Evaluation: eval, Alerts: alerts, RecentAlerts: recent}, nil
}

func safetyActionBlocked(view *abSafetyView) bool { return view != nil && view.Evaluation.HardBlock }
func safetyRollbackRecommended(view *abSafetyView) bool {
	return view != nil && view.Evaluation.RollbackRecommended
}