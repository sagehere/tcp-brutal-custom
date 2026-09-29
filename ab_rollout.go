package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type abRolloutPlan struct {
	Stages                   []uint8 `json:"stages"`
	ObservationWindowSeconds uint64  `json:"observation_window_seconds"`
}

func defaultABRolloutPlan(initial uint8) *abRolloutPlan {
	if initial == 0 || initial >= 100 {
		return nil
	}
	base := []uint8{5, 10, 25, 50, 100}
	stages := []uint8{initial}
	for _, p := range base {
		if p > initial {
			stages = append(stages, p)
		}
	}
	return &abRolloutPlan{Stages: stages, ObservationWindowSeconds: 3600}
}

func effectiveABRolloutPlan(in *abRolloutPlan, initial uint8) (*abRolloutPlan, error) {
	if in == nil {
		return nil, nil
	}
	p := &abRolloutPlan{Stages: append([]uint8(nil), in.Stages...), ObservationWindowSeconds: in.ObservationWindowSeconds}
	if len(p.Stages) == 0 || len(p.Stages) > 20 {
		return nil, errors.New("rollout stages must contain 1 to 20 percentages")
	}
	if p.ObservationWindowSeconds < 60 || p.ObservationWindowSeconds > 30*24*3600 {
		return nil, errors.New("observation window must be between 60 seconds and 30 days")
	}
	var previous uint8
	currentFound := initial == 0
	for i, v := range p.Stages {
		if v == 0 || v > 100 {
			return nil, fmt.Errorf("invalid rollout stage %d", v)
		}
		if i > 0 && v <= previous {
			return nil, errors.New("rollout stages must be strictly increasing")
		}
		if v == initial {
			currentFound = true
		}
		previous = v
	}
	if !currentFound {
		return nil, errors.New("current canary percentage is not present in rollout stages")
	}
	return p, nil
}

type abRollout struct {
	ID                       int64   `json:"id"`
	Port                     uint16  `json:"port"`
	Created                  int64   `json:"created"`
	Ended                    int64   `json:"ended,omitempty"`
	Status                   string  `json:"status"`
	Stages                   []uint8 `json:"stages"`
	ObservationWindowSeconds uint64  `json:"observation_window_seconds"`
	CurrentStageIndex        int     `json:"current_stage_index"`
	CurrentEpochID           int64   `json:"current_epoch_id,omitempty"`
	CurrentStageID           int64   `json:"current_stage_id,omitempty"`
}

type abRolloutStage struct {
	ID              int64  `json:"id"`
	RolloutID       int64  `json:"rollout_id"`
	StageIndex      int    `json:"stage_index"`
	CanaryPercent   uint8  `json:"canary_percent"`
	EpochID         int64  `json:"epoch_id"`
	Started         int64  `json:"started"`
	ObservationEnds int64  `json:"observation_ends"`
	Ended           int64  `json:"ended,omitempty"`
	EndReason       string `json:"end_reason,omitempty"`
	FinalState      string `json:"final_state,omitempty"`
}

type abRolloutEvent struct {
	ID          int64  `json:"id"`
	RolloutID   int64  `json:"rollout_id"`
	Port        uint16 `json:"port"`
	Time        int64  `json:"time"`
	Action      string `json:"action"`
	FromPercent uint8  `json:"from_percent"`
	ToPercent   uint8  `json:"to_percent"`
	EpochID     int64  `json:"epoch_id,omitempty"`
	Detail      string `json:"detail,omitempty"`
}

type abRolloutView struct {
	Rollout          abRollout        `json:"rollout"`
	CurrentStage     *abRolloutStage  `json:"current_stage,omitempty"`
	StageHistory     []abRolloutStage `json:"stage_history"`
	Events           []abRolloutEvent `json:"events"`
	WindowState      string           `json:"window_state"`
	SecondsRemaining int64            `json:"seconds_remaining"`
	Evaluation       *abEpochAnalysis `json:"evaluation,omitempty"`
	AllowedActions   []string         `json:"allowed_actions,omitempty"`
}

func (h *history) beginABRollout(p abPortConfig, epochID int64) (*abRollout, error) {
	if p.RolloutPlan == nil {
		return nil, nil
	}
	plan, err := effectiveABRolloutPlan(p.RolloutPlan, p.CanaryPercent)
	if err != nil {
		return nil, err
	}
	now := time.Now().Unix()
	deadline := now + int64(plan.ObservationWindowSeconds)
	stagesJSON, _ := json.Marshal(plan.Stages)
	tx, err := h.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err = tx.Exec("UPDATE ab_rollouts SET ended_ts=?,status='superseded' WHERE port=? AND ended_ts IS NULL", now, p.Port); err != nil {
		return nil, err
	}
	res, err := tx.Exec("INSERT INTO ab_rollouts(port,created_ts,status,stages_json,observation_window_seconds,current_stage_index,current_epoch_id,current_stage_id) VALUES(?,?,?,?,?,?,?,0)",
		p.Port, now, "active", string(stagesJSON), plan.ObservationWindowSeconds, 0, epochID)
	if err != nil {
		return nil, err
	}
	rolloutID, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	res, err = tx.Exec("INSERT INTO ab_rollout_stages(rollout_id,stage_index,canary_percent,epoch_id,started_ts,observation_ends_ts) VALUES(?,?,?,?,?,?)",
		rolloutID, 0, p.CanaryPercent, epochID, now, deadline)
	if err != nil {
		return nil, err
	}
	stageID, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	if _, err = tx.Exec("UPDATE ab_rollouts SET current_stage_id=? WHERE id=?", stageID, rolloutID); err != nil {
		return nil, err
	}
	if _, err = tx.Exec("INSERT INTO ab_rollout_events(rollout_id,port,ts,action,from_percent,to_percent,epoch_id,detail) VALUES(?,?,?,?,?,?,?,?)",
		rolloutID, p.Port, now, "create", 0, p.CanaryPercent, epochID, "rollout created"); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return h.abRolloutByID(rolloutID)
}

func scanABRollout(row interface{ Scan(...any) error }) (*abRollout, error) {
	var x abRollout
	var stagesJSON string
	if err := row.Scan(&x.ID, &x.Port, &x.Created, &x.Ended, &x.Status, &stagesJSON, &x.ObservationWindowSeconds, &x.CurrentStageIndex, &x.CurrentEpochID, &x.CurrentStageID); err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(stagesJSON), &x.Stages); err != nil {
		return nil, err
	}
	return &x, nil
}

const abRolloutSelect = "SELECT id,port,created_ts,COALESCE(ended_ts,0),status,stages_json,observation_window_seconds,current_stage_index,COALESCE(current_epoch_id,0),COALESCE(current_stage_id,0) FROM ab_rollouts"

func (h *history) abRolloutByID(id int64) (*abRollout, error) {
	return scanABRollout(h.db.QueryRow(abRolloutSelect+" WHERE id=?", id))
}

func (h *history) latestABRollout(port uint16) (*abRollout, error) {
	x, err := scanABRollout(h.db.QueryRow(abRolloutSelect+" WHERE port=? ORDER BY id DESC LIMIT 1", port))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return x, err
}

func (h *history) activeABRollout(port uint16) (*abRollout, error) {
	x, err := scanABRollout(h.db.QueryRow(abRolloutSelect+" WHERE port=? AND ended_ts IS NULL ORDER BY id DESC LIMIT 1", port))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return x, err
}

func (h *history) abRolloutStage(id int64) (*abRolloutStage, error) {
	var x abRolloutStage
	err := h.db.QueryRow("SELECT id,rollout_id,stage_index,canary_percent,epoch_id,started_ts,observation_ends_ts,COALESCE(ended_ts,0),COALESCE(end_reason,''),COALESCE(final_state,'') FROM ab_rollout_stages WHERE id=?", id).
		Scan(&x.ID, &x.RolloutID, &x.StageIndex, &x.CanaryPercent, &x.EpochID, &x.Started, &x.ObservationEnds, &x.Ended, &x.EndReason, &x.FinalState)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return &x, err
}

func (h *history) abRolloutStages(rolloutID int64) ([]abRolloutStage, error) {
	rows, err := h.db.Query("SELECT id,rollout_id,stage_index,canary_percent,epoch_id,started_ts,observation_ends_ts,COALESCE(ended_ts,0),COALESCE(end_reason,''),COALESCE(final_state,'') FROM ab_rollout_stages WHERE rollout_id=? ORDER BY id", rolloutID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []abRolloutStage
	for rows.Next() {
		var x abRolloutStage
		if err := rows.Scan(&x.ID, &x.RolloutID, &x.StageIndex, &x.CanaryPercent, &x.EpochID, &x.Started, &x.ObservationEnds, &x.Ended, &x.EndReason, &x.FinalState); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

func (h *history) abRolloutEvents(rolloutID int64) ([]abRolloutEvent, error) {
	rows, err := h.db.Query("SELECT id,rollout_id,port,ts,action,from_percent,to_percent,COALESCE(epoch_id,0),COALESCE(detail,'') FROM ab_rollout_events WHERE rollout_id=? ORDER BY id DESC", rolloutID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []abRolloutEvent
	for rows.Next() {
		var x abRolloutEvent
		if err := rows.Scan(&x.ID, &x.RolloutID, &x.Port, &x.Time, &x.Action, &x.FromPercent, &x.ToPercent, &x.EpochID, &x.Detail); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

type abRolloutArchive struct {
	Rollout abRollout        `json:"rollout"`
	Stages  []abRolloutStage `json:"stages"`
	Events  []abRolloutEvent `json:"events"`
}

func (h *history) abRolloutArchives(port uint16, from, to int64) ([]abRolloutArchive, error) {
	rows, err := h.db.Query(abRolloutSelect+" WHERE port=? AND created_ts<? AND COALESCE(ended_ts,?)>=? ORDER BY id", port, to, to, from)
	if err != nil {
		return nil, err
	}
	var rollouts []abRollout
	for rows.Next() {
		r, err := scanABRollout(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		rollouts = append(rollouts, *r)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	out := make([]abRolloutArchive, 0, len(rollouts))
	for _, r := range rollouts {
		stages, err := h.abRolloutStages(r.ID)
		if err != nil {
			return nil, err
		}
		events, err := h.abRolloutEvents(r.ID)
		if err != nil {
			return nil, err
		}
		out = append(out, abRolloutArchive{Rollout: r, Stages: stages, Events: events})
	}
	return out, nil
}

func (h *history) addABRolloutEvent(rolloutID int64, port uint16, action string, from, to uint8, epochID int64, detail string) error {
	_, err := h.db.Exec("INSERT INTO ab_rollout_events(rollout_id,port,ts,action,from_percent,to_percent,epoch_id,detail) VALUES(?,?,?,?,?,?,?,?)",
		rolloutID, port, time.Now().Unix(), action, from, to, epochID, detail)
	return err
}

func (h *history) closeABRolloutStage(stageID int64, finalState, reason string) error {
	if stageID == 0 {
		return nil
	}
	_, err := h.db.Exec("UPDATE ab_rollout_stages SET ended_ts=?,end_reason=?,final_state=? WHERE id=? AND ended_ts IS NULL",
		time.Now().Unix(), reason, finalState, stageID)
	return err
}

func (h *history) startABRolloutStage(r *abRollout, stageIndex int, percent uint8, epochID int64) (*abRolloutStage, error) {
	now := time.Now().Unix()
	deadline := now + int64(r.ObservationWindowSeconds)
	tx, err := h.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	res, err := tx.Exec("INSERT INTO ab_rollout_stages(rollout_id,stage_index,canary_percent,epoch_id,started_ts,observation_ends_ts) VALUES(?,?,?,?,?,?)",
		r.ID, stageIndex, percent, epochID, now, deadline)
	if err != nil {
		return nil, err
	}
	stageID, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	if _, err = tx.Exec("UPDATE ab_rollouts SET status='active',current_stage_index=?,current_epoch_id=?,current_stage_id=? WHERE id=? AND ended_ts IS NULL",
		stageIndex, epochID, stageID, r.ID); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return h.abRolloutStage(stageID)
}

func (h *history) setABRolloutStatus(id int64, status string, ended bool) error {
	if ended {
		_, err := h.db.Exec("UPDATE ab_rollouts SET status=?,ended_ts=? WHERE id=? AND ended_ts IS NULL", status, time.Now().Unix(), id)
		return err
	}
	_, err := h.db.Exec("UPDATE ab_rollouts SET status=? WHERE id=? AND ended_ts IS NULL", status, id)
	return err
}

func (h *history) reconcileABRollout(p abPortConfig, epochID int64) error {
	r, err := h.activeABRollout(p.Port)
	if err != nil || r == nil || r.Status != "active" || r.CurrentEpochID == epochID {
		return err
	}
	if r.CurrentStageIndex < 0 || r.CurrentStageIndex >= len(r.Stages) || r.Stages[r.CurrentStageIndex] != p.CanaryPercent {
		return h.setABRolloutStatus(r.ID, "desynced", true)
	}
	if err := h.closeABRolloutStage(r.CurrentStageID, "restarted", "manager_restart_new_epoch"); err != nil {
		return err
	}
	stage, err := h.startABRolloutStage(r, r.CurrentStageIndex, p.CanaryPercent, epochID)
	if err != nil {
		return err
	}
	return h.addABRolloutEvent(r.ID, p.Port, "restart_window", p.CanaryPercent, p.CanaryPercent, epochID, fmt.Sprintf("new stage window %d", stage.ID))
}

func (m *manager) evaluateRolloutStage(port uint16, stage *abRolloutStage) (*abEpochAnalysis, error) {
	if stage == nil {
		return nil, nil
	}
	summaries, err := m.history.abSummaries(port, stage.Started, stage.ObservationEnds)
	if err != nil {
		return nil, err
	}
	epochs, err := m.history.abEpochs(port, stage.Started, stage.ObservationEnds)
	if err != nil {
		return nil, err
	}
	samples, err := m.history.abSamples("minute", port, stage.Started, stage.ObservationEnds)
	if err != nil {
		return nil, err
	}
	plans, err := m.history.abPlans(port, stage.Started, stage.ObservationEnds)
	if err != nil {
		return nil, err
	}
	comparisons := buildABComparisons(summaries, defaultABAnalysisPolicy())
	analyses := buildABEpochAnalyses(epochs, summaries, comparisons, samples, plans)
	for i := range analyses {
		if analyses[i].EpochID == stage.EpochID {
			return &analyses[i], nil
		}
	}
	return nil, nil
}

func (m *manager) rolloutView(port uint16) (*abRolloutView, error) {
	r, err := m.history.latestABRollout(port)
	if err != nil || r == nil {
		return nil, err
	}
	stages, err := m.history.abRolloutStages(r.ID)
	if err != nil {
		return nil, err
	}
	events, err := m.history.abRolloutEvents(r.ID)
	if err != nil {
		return nil, err
	}
	stage, err := m.history.abRolloutStage(r.CurrentStageID)
	if err != nil {
		return nil, err
	}
	view := &abRolloutView{Rollout: *r, CurrentStage: stage, StageHistory: stages, Events: events}
	switch r.Status {
	case "paused":
		view.WindowState = "paused"
		view.AllowedActions = []string{"resume", "rollback"}
		return view, nil
	case "rolled_back", "completed", "stopped", "superseded", "desynced":
		view.WindowState = r.Status
		return view, nil
	}
	if stage == nil {
		view.WindowState = "desynced"
		return view, nil
	}
	now := time.Now().Unix()
	view.SecondsRemaining = stage.ObservationEnds - now
	if view.SecondsRemaining > 0 {
		view.WindowState = "observing"
		view.AllowedActions = []string{"pause", "rollback"}
		return view, nil
	}
	view.SecondsRemaining = 0
	last := r.CurrentStageIndex == len(r.Stages)-1
	if stage.CanaryPercent == 100 && last {
		view.WindowState = "completion_review"
		view.AllowedActions = []string{"complete", "rollback", "retry"}
		return view, nil
	}
	evaluation, err := m.evaluateRolloutStage(port, stage)
	if err != nil {
		return nil, err
	}
	view.Evaluation = evaluation
	if evaluation == nil {
		view.WindowState = "insufficient_review"
		view.AllowedActions = []string{"retry", "pause", "rollback"}
		return view, nil
	}
	switch evaluation.State {
	case "eligible_review":
		if last {
			view.WindowState = "completion_review"
			view.AllowedActions = []string{"complete", "rollback", "retry"}
		} else {
			view.WindowState = "eligible_review"
			view.AllowedActions = []string{"advance", "retry", "pause", "rollback"}
		}
	case "guardrail_review":
		view.WindowState = "guardrail_review"
		view.AllowedActions = []string{"retry", "pause", "rollback"}
	case "network_review_only":
		view.WindowState = "network_review_only"
		view.AllowedActions = []string{"retry", "pause", "rollback"}
	default:
		view.WindowState = "insufficient_review"
		view.AllowedActions = []string{"retry", "pause", "rollback"}
	}
	return view, nil
}

func (m *manager) abRolloutAPI(w http.ResponseWriter, r *http.Request) {
	pv, err := strconv.ParseUint(r.URL.Query().Get("port"), 10, 16)
	if err != nil || pv == 0 {
		bad(w, 400, errors.New("valid port is required"))
		return
	}
	view, err := m.rolloutView(uint16(pv))
	if err != nil {
		bad(w, 500, err)
		return
	}
	if view == nil {
		bad(w, 404, errors.New("rollout not found"))
		return
	}
	jsonReply(w, 200, view)
}

func rolloutActionPath(path string) (uint16, string, error) {
	parts := strings.Split(strings.TrimPrefix(path, "/api/v1/ab/rollout/"), "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return 0, "", errors.New("invalid rollout action path")
	}
	n, err := strconv.ParseUint(parts[0], 10, 16)
	if err != nil || n == 0 {
		return 0, "", errors.New("invalid port")
	}
	switch parts[1] {
	case "pause", "resume", "rollback", "retry", "advance", "complete":
	default:
		return 0, "", errors.New("invalid rollout action")
	}
	return uint16(n), parts[1], nil
}

func (m *manager) setABPercentLocked(idx int, percent uint8, reason string) (abPortConfig, int64, error) {
	previous := m.cfg.ABPorts[idx]
	next := m.cfg
	next.ABPorts = append([]abPortConfig(nil), m.cfg.ABPorts...)
	next.ABPorts[idx].CanaryPercent = percent
	if err := m.applyABPort(next.ABPorts[idx]); err != nil {
		return previous, 0, err
	}
	if err := saveConfig(next); err != nil {
		_ = m.applyABPort(previous)
		return previous, 0, err
	}
	m.cfg = next
	epochID, err := m.history.beginABEpoch(next.ABPorts[idx], reason)
	if err != nil {
		return next.ABPorts[idx], 0, err
	}
	if err = m.seedABNow(next.ABPorts[idx]); err != nil {
		return next.ABPorts[idx], epochID, err
	}
	return next.ABPorts[idx], epochID, nil
}

func (m *manager) abRolloutActionAPI(w http.ResponseWriter, r *http.Request) {
	port, action, err := rolloutActionPath(r.URL.Path)
	if err != nil {
		bad(w, 400, err)
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	idx := -1
	for i := range m.cfg.ABPorts {
		if m.cfg.ABPorts[i].Port == port {
			idx = i
			break
		}
	}
	if idx < 0 {
		bad(w, 404, errors.New("A/B port not configured"))
		return
	}
	rollout, err := m.history.activeABRollout(port)
	if err != nil {
		bad(w, 500, err)
		return
	}
	if rollout == nil {
		bad(w, 409, errors.New("no active rollout"))
		return
	}
	view, err := m.rolloutView(port)
	if err != nil {
		bad(w, 500, err)
		return
	}
	allowed := false
	for _, v := range view.AllowedActions {
		if v == action {
			allowed = true
			break
		}
	}
	if !allowed {
		bad(w, 409, fmt.Errorf("action %s is not allowed while rollout state is %s", action, view.WindowState))
		return
	}
	current := m.cfg.ABPorts[idx].CanaryPercent
	switch action {
	case "pause":
		_, epochID, err := m.setABPercentLocked(idx, 0, "rollout_pause")
		if err != nil {
			bad(w, 500, err)
			return
		}
		if err = m.history.closeABRolloutStage(rollout.CurrentStageID, "paused", "manual_pause"); err != nil {
			bad(w, 500, err)
			return
		}
		if err = m.history.setABRolloutStatus(rollout.ID, "paused", false); err != nil {
			bad(w, 500, err)
			return
		}
		_ = m.history.addABRolloutEvent(rollout.ID, port, "pause", current, 0, epochID, "manual pause to baseline")
	case "resume":
		target := rollout.Stages[rollout.CurrentStageIndex]
		_, epochID, err := m.setABPercentLocked(idx, target, "rollout_resume")
		if err != nil {
			bad(w, 500, err)
			return
		}
		stage, err := m.history.startABRolloutStage(rollout, rollout.CurrentStageIndex, target, epochID)
		if err != nil {
			bad(w, 500, err)
			return
		}
		_ = m.history.addABRolloutEvent(rollout.ID, port, "resume", current, target, epochID, fmt.Sprintf("new observation window stage %d", stage.ID))
	case "rollback":
		epochID := int64(0)
		if current != 0 {
			_, epochID, err = m.setABPercentLocked(idx, 0, "rollout_rollback")
			if err != nil {
				bad(w, 500, err)
				return
			}
		}
		if rollout.Status == "active" {
			_ = m.history.closeABRolloutStage(rollout.CurrentStageID, "rolled_back", "manual_rollback")
		}
		if err = m.history.setABRolloutStatus(rollout.ID, "rolled_back", true); err != nil {
			bad(w, 500, err)
			return
		}
		_ = m.history.addABRolloutEvent(rollout.ID, port, "rollback", current, 0, epochID, "manual rollback to baseline")
	case "retry":
		target := rollout.Stages[rollout.CurrentStageIndex]
		if err = m.history.closeABRolloutStage(rollout.CurrentStageID, view.WindowState, "manual_retry"); err != nil {
			bad(w, 500, err)
			return
		}
		_, epochID, err := m.setABPercentLocked(idx, target, "rollout_retry")
		if err != nil {
			bad(w, 500, err)
			return
		}
		stage, err := m.history.startABRolloutStage(rollout, rollout.CurrentStageIndex, target, epochID)
		if err != nil {
			bad(w, 500, err)
			return
		}
		_ = m.history.addABRolloutEvent(rollout.ID, port, "retry", current, target, epochID, fmt.Sprintf("new observation window stage %d", stage.ID))
	case "advance":
		if rollout.CurrentStageIndex+1 >= len(rollout.Stages) {
			bad(w, 409, errors.New("no next rollout stage"))
			return
		}
		nextIndex := rollout.CurrentStageIndex + 1
		target := rollout.Stages[nextIndex]
		if err = m.history.closeABRolloutStage(rollout.CurrentStageID, "eligible_review", "manual_advance"); err != nil {
			bad(w, 500, err)
			return
		}
		_, epochID, err := m.setABPercentLocked(idx, target, "rollout_advance")
		if err != nil {
			bad(w, 500, err)
			return
		}
		stage, err := m.history.startABRolloutStage(rollout, nextIndex, target, epochID)
		if err != nil {
			bad(w, 500, err)
			return
		}
		_ = m.history.addABRolloutEvent(rollout.ID, port, "advance", current, target, epochID, fmt.Sprintf("manual approval; new stage %d", stage.ID))
	case "complete":
		if err = m.history.closeABRolloutStage(rollout.CurrentStageID, "completed", "manual_complete"); err != nil {
			bad(w, 500, err)
			return
		}
		if err = m.history.setABRolloutStatus(rollout.ID, "completed", true); err != nil {
			bad(w, 500, err)
			return
		}
		_ = m.history.addABRolloutEvent(rollout.ID, port, "complete", current, current, rollout.CurrentEpochID, "manual rollout completion")
	}
	out, err := m.rolloutView(port)
	if err != nil {
		bad(w, 500, err)
		return
	}
	jsonReply(w, 200, out)
}