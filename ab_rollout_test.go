package main

import (
	"testing"
	"time"
)

func hasRolloutAction(actions []string, want string) bool {
	for _, action := range actions {
		if action == want {
			return true
		}
	}
	return false
}

func TestABRolloutViewDoesNotAdvanceBeforeDeadline(t *testing.T) {
	dir := t.TempDir()
	h, err := openHistoryAt(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer h.close()

	analysis := defaultABExperimentPlan()
	p := abPortConfig{
		Port: 8443, RateMbps: 100, Gain: 20, CanaryPercent: 5, Enabled: true,
		AnalysisPlan: &analysis,
		RolloutPlan:  &abRolloutPlan{Stages: []uint8{5, 10, 25}, ObservationWindowSeconds: 3600},
	}
	epochID, err := h.beginABEpoch(p, "rollout_test")
	if err != nil {
		t.Fatal(err)
	}
	rollout, err := h.beginABRollout(p, epochID)
	if err != nil {
		t.Fatal(err)
	}
	m := &manager{history: h}
	view, err := m.rolloutView(p.Port)
	if err != nil {
		t.Fatal(err)
	}
	if view.WindowState != "observing" {
		t.Fatalf("window state=%q", view.WindowState)
	}
	if !hasRolloutAction(view.AllowedActions, "pause") || !hasRolloutAction(view.AllowedActions, "rollback") {
		t.Fatalf("observing actions=%v", view.AllowedActions)
	}
	if hasRolloutAction(view.AllowedActions, "advance") {
		t.Fatalf("advance allowed before deadline: %v", view.AllowedActions)
	}

	if _, err = h.db.Exec("UPDATE ab_rollout_stages SET observation_ends_ts=? WHERE id=?", time.Now().Unix()-1, rollout.CurrentStageID); err != nil {
		t.Fatal(err)
	}
	view, err = m.rolloutView(p.Port)
	if err != nil {
		t.Fatal(err)
	}
	if view.WindowState != "insufficient_review" {
		t.Fatalf("expired empty window state=%q evaluation=%+v", view.WindowState, view.Evaluation)
	}
	if !hasRolloutAction(view.AllowedActions, "retry") {
		t.Fatalf("retry missing after insufficient window: %v", view.AllowedActions)
	}
	if hasRolloutAction(view.AllowedActions, "advance") {
		t.Fatalf("advance allowed for insufficient window: %v", view.AllowedActions)
	}
}

func TestABRolloutPauseStateOffersResumeNotAdvance(t *testing.T) {
	dir := t.TempDir()
	h, err := openHistoryAt(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer h.close()

	analysis := defaultABExperimentPlan()
	p := abPortConfig{
		Port: 9443, RateMbps: 100, Gain: 20, CanaryPercent: 5, Enabled: true,
		AnalysisPlan: &analysis,
		RolloutPlan:  &abRolloutPlan{Stages: []uint8{5, 10}, ObservationWindowSeconds: 3600},
	}
	epochID, err := h.beginABEpoch(p, "rollout_pause_test")
	if err != nil {
		t.Fatal(err)
	}
	rollout, err := h.beginABRollout(p, epochID)
	if err != nil {
		t.Fatal(err)
	}
	if err = h.closeABRolloutStage(rollout.CurrentStageID, "paused", "test"); err != nil {
		t.Fatal(err)
	}
	if err = h.setABRolloutStatus(rollout.ID, "paused", false); err != nil {
		t.Fatal(err)
	}

	m := &manager{history: h}
	view, err := m.rolloutView(p.Port)
	if err != nil {
		t.Fatal(err)
	}
	if view.WindowState != "paused" || !hasRolloutAction(view.AllowedActions, "resume") || !hasRolloutAction(view.AllowedActions, "rollback") {
		t.Fatalf("paused view=%+v", view)
	}
	if hasRolloutAction(view.AllowedActions, "advance") {
		t.Fatalf("advance allowed while paused: %v", view.AllowedActions)
	}
}

func TestABRolloutReconcileRestartsWindowForNewEpoch(t *testing.T) {
	dir := t.TempDir()
	h, err := openHistoryAt(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer h.close()

	analysis := defaultABExperimentPlan()
	p := abPortConfig{
		Port: 10443, RateMbps: 100, Gain: 20, CanaryPercent: 5, Enabled: true,
		AnalysisPlan: &analysis,
		RolloutPlan:  &abRolloutPlan{Stages: []uint8{5, 10}, ObservationWindowSeconds: 3600},
	}
	epoch1, err := h.beginABEpoch(p, "rollout_restart_test")
	if err != nil {
		t.Fatal(err)
	}
	rollout, err := h.beginABRollout(p, epoch1)
	if err != nil {
		t.Fatal(err)
	}
	stage1, err := h.abRolloutStage(rollout.CurrentStageID)
	if err != nil {
		t.Fatal(err)
	}
	epoch2, err := h.beginABEpoch(p, "simulated_code_upgrade")
	if err != nil {
		t.Fatal(err)
	}
	if err = h.reconcileABRollout(p, epoch2); err != nil {
		t.Fatal(err)
	}
	current, err := h.activeABRollout(p.Port)
	if err != nil || current == nil {
		t.Fatalf("active rollout=%+v err=%v", current, err)
	}
	if current.CurrentEpochID != epoch2 || current.CurrentStageID == stage1.ID || current.CurrentStageIndex != 0 {
		t.Fatalf("rollout did not move to new attempt: %+v", current)
	}
	stages, err := h.abRolloutStages(rollout.ID)
	if err != nil || len(stages) != 2 {
		t.Fatalf("stages=%+v err=%v", stages, err)
	}
	if stages[0].FinalState != "restarted" || stages[0].EndReason != "manager_restart_new_epoch" {
		t.Fatalf("previous stage not closed as restart: %+v", stages[0])
	}
	if stages[1].EpochID != epoch2 || stages[1].ObservationEnds-stages[1].Started != 3600 {
		t.Fatalf("replacement window=%+v", stages[1])
	}
	events, err := h.abRolloutEvents(rollout.ID)
	if err != nil || len(events) < 2 || events[0].Action != "restart_window" {
		t.Fatalf("events=%+v err=%v", events, err)
	}
}