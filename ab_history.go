package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"time"
)

type abEpoch struct {
	ID            int64   `json:"id"`
	Port          uint16  `json:"port"`
	Started       int64   `json:"started"`
	Ended         int64   `json:"ended,omitempty"`
	CanaryPercent uint8   `json:"canary_percent"`
	RateMbps      float64 `json:"rate_mbps"`
	Gain          uint32  `json:"gain"`
	CodeVersion   string  `json:"code_version"`
	Reason        string  `json:"reason"`
}

type abCohortSample struct {
	Time          int64  `json:"time"`
	EpochID       int64  `json:"epoch_id"`
	Port          uint16 `json:"port"`
	Cohort        string `json:"cohort"`
	Group         uint64 `json:"group"`
	Duration      uint64 `json:"duration_seconds"`
	MemberSeconds uint64 `json:"member_seconds"`
	Sent          uint64 `json:"sent"`
	Acked         uint64 `json:"acked"`
	Retrans       uint64 `json:"retrans"`
	Members       uint32 `json:"members"`
	RTTSum        uint64 `json:"rtt_sum_us"`
	RTTSamples    uint64 `json:"rtt_samples"`
	RTTMax        uint32 `json:"rtt_max_us"`
	Gap           bool   `json:"gap"`
}

type abSelectorSample struct {
	Time     int64  `json:"time"`
	EpochID  int64  `json:"epoch_id"`
	Port     uint16 `json:"port"`
	Baseline uint64 `json:"baseline"`
	Canary   uint64 `json:"canary"`
	Failure  uint64 `json:"failure"`
	Gap      bool   `json:"gap"`
}

type abAppSample struct {
	Time           int64  `json:"time"`
	EpochID        int64  `json:"epoch_id"`
	Port           uint16 `json:"port"`
	Cohort         string `json:"cohort"`
	Source         string `json:"source"`
	Requests       uint64 `json:"requests"`
	Success        uint64 `json:"success"`
	Errors         uint64 `json:"errors"`
	LatencySumUS   uint64 `json:"latency_sum_us"`
	LatencySamples uint64 `json:"latency_samples"`
	LatencyMaxUS   uint64 `json:"latency_max_us"`
}

type abSummary struct {
	EpochID               int64   `json:"epoch_id"`
	Port                  uint16  `json:"port"`
	Cohort                string  `json:"cohort"`
	CanaryPercent         uint8   `json:"canary_percent"`
	DurationSeconds       uint64  `json:"duration_seconds"`
	MemberSeconds         uint64  `json:"member_seconds"`
	SentBytes             uint64  `json:"sent_bytes"`
	AckedBytes            uint64  `json:"acked_bytes"`
	RetransBytes          uint64  `json:"retrans_bytes"`
	AssignedConnections   uint64  `json:"assigned_connections"`
	SelectorFailures      uint64  `json:"selector_failures"`
	RTTSamples            uint64  `json:"rtt_samples"`
	MeanRTTMS             float64 `json:"mean_rtt_ms"`
	MaxRTTMS              float64 `json:"max_rtt_ms"`
	RetransPercent        float64 `json:"retrans_percent"`
	GoodputMbps           float64 `json:"goodput_mbps"`
	GoodputPerMemberMbps  float64 `json:"goodput_per_member_mbps"`
	AppRequests           uint64  `json:"app_requests"`
	AppSuccess            uint64  `json:"app_success"`
	AppErrors             uint64  `json:"app_errors"`
	AppSuccessPercent     float64 `json:"app_success_percent"`
	AppMeanLatencyMS      float64 `json:"app_mean_latency_ms"`
	AppMaxLatencyMS       float64 `json:"app_max_latency_ms"`
	GapSamples            uint64  `json:"gap_samples"`
}

func (h *history) initAB() error {
	for _, q := range []string{
		`CREATE TABLE IF NOT EXISTS ab_meta (key TEXT PRIMARY KEY, value TEXT NOT NULL)`,
		`INSERT OR IGNORE INTO ab_meta(key,value) VALUES('schema_version','1')`,
		`CREATE TABLE IF NOT EXISTS ab_epochs (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			port INTEGER NOT NULL,
			started_ts INTEGER NOT NULL,
			ended_ts INTEGER,
			canary_percent INTEGER NOT NULL,
			rate_mbps REAL NOT NULL,
			gain INTEGER NOT NULL,
			code_version TEXT NOT NULL,
			reason TEXT NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS ab_epochs_port_time ON ab_epochs(port,started_ts)`,
		`CREATE TABLE IF NOT EXISTS ab_samples (
			tier TEXT NOT NULL, ts INTEGER NOT NULL, epoch_id INTEGER NOT NULL,
			port INTEGER NOT NULL, cohort TEXT NOT NULL, group_id INTEGER NOT NULL,
			duration_sec INTEGER NOT NULL, member_seconds INTEGER NOT NULL,
			sent INTEGER NOT NULL, acked INTEGER NOT NULL, retrans INTEGER NOT NULL,
			members INTEGER NOT NULL, rtt_sum INTEGER NOT NULL, rtt_samples INTEGER NOT NULL,
			rtt_max INTEGER NOT NULL, gap INTEGER NOT NULL,
			PRIMARY KEY(tier,ts,epoch_id,cohort,group_id)
		)`,
		`CREATE INDEX IF NOT EXISTS ab_samples_query ON ab_samples(tier,port,ts,epoch_id,cohort)`,
		`CREATE TABLE IF NOT EXISTS ab_selector_samples (
			tier TEXT NOT NULL, ts INTEGER NOT NULL, epoch_id INTEGER NOT NULL, port INTEGER NOT NULL,
			baseline INTEGER NOT NULL, canary INTEGER NOT NULL, failure INTEGER NOT NULL, gap INTEGER NOT NULL,
			PRIMARY KEY(tier,ts,epoch_id,port)
		)`,
		`CREATE TABLE IF NOT EXISTS ab_checkpoints (
			port INTEGER NOT NULL, cohort TEXT NOT NULL, group_id INTEGER NOT NULL, ts INTEGER NOT NULL,
			sent INTEGER NOT NULL, acked INTEGER NOT NULL, retrans INTEGER NOT NULL,
			rtt_sum INTEGER NOT NULL, rtt_samples INTEGER NOT NULL,
			PRIMARY KEY(port,cohort,group_id)
		)`,
		`CREATE TABLE IF NOT EXISTS ab_selector_checkpoints (
			port INTEGER PRIMARY KEY, baseline INTEGER NOT NULL, canary INTEGER NOT NULL, failure INTEGER NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS ab_app_samples (
			tier TEXT NOT NULL, ts INTEGER NOT NULL, epoch_id INTEGER NOT NULL, port INTEGER NOT NULL,
			cohort TEXT NOT NULL, source TEXT NOT NULL,
			requests INTEGER NOT NULL, success INTEGER NOT NULL, errors INTEGER NOT NULL,
			latency_sum_us INTEGER NOT NULL, latency_samples INTEGER NOT NULL, latency_max_us INTEGER NOT NULL,
			PRIMARY KEY(tier,ts,epoch_id,cohort,source)
		)`,
		`CREATE INDEX IF NOT EXISTS ab_app_query ON ab_app_samples(tier,port,ts,epoch_id,cohort)`,
		`CREATE TABLE IF NOT EXISTS ab_epoch_plans (
			epoch_id INTEGER PRIMARY KEY,
			alpha REAL NOT NULL, power REAL NOT NULL, expected_app_success_percent REAL NOT NULL,
			app_success_ni_margin_pp REAL NOT NULL, max_retrans_delta_pp REAL NOT NULL,
			max_mean_rtt_delta_percent REAL NOT NULL, min_goodput_delta_percent REAL NOT NULL,
			bootstrap_block_minutes INTEGER NOT NULL, predeclared INTEGER NOT NULL
		)`,
	} {
		if _, err := h.db.Exec(q); err != nil {
			return err
		}
	}
	var schemaVersion string
	if err := h.db.QueryRow("SELECT value FROM ab_meta WHERE key='schema_version'").Scan(&schemaVersion); err != nil {
		return err
	}
	switch schemaVersion {
	case "1":
		if _, err := h.db.Exec("UPDATE ab_meta SET value='2' WHERE key='schema_version'"); err != nil {
			return err
		}
	case "2":
	default:
		return fmt.Errorf("unsupported A/B schema version %q", schemaVersion)
	}
	if h.abLast == nil {
		h.abLast = map[string]abCohortSample{}
	}
	if h.abSelectorLast == nil {
		h.abSelectorLast = map[uint16]selectorCount{}
	}
	if h.abEpoch == nil {
		h.abEpoch = map[uint16]int64{}
	}
	rows, err := h.db.Query("SELECT port,cohort,group_id,ts,sent,acked,retrans,rtt_sum,rtt_samples FROM ab_checkpoints")
	if err != nil {
		return err
	}
	for rows.Next() {
		var x abCohortSample
		if err := rows.Scan(&x.Port, &x.Cohort, &x.Group, &x.Time, &x.Sent, &x.Acked, &x.Retrans, &x.RTTSum, &x.RTTSamples); err != nil {
			rows.Close()
			return err
		}
		h.abLast[abCohortKey(x.Port, x.Cohort, x.Group)] = x
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	rows, err = h.db.Query("SELECT port,baseline,canary,failure FROM ab_selector_checkpoints")
	if err != nil {
		return err
	}
	for rows.Next() {
		var port uint16
		var c selectorCount
		if err := rows.Scan(&port, &c.Baseline, &c.Canary, &c.Failure); err != nil {
			rows.Close()
			return err
		}
		h.abSelectorLast[port] = c
	}
	err = rows.Err()
	rows.Close()
	return err
}

func abCohortKey(port uint16, cohort string, group uint64) string {
	return fmt.Sprintf("%d/%s/%d", port, cohort, group)
}

func (h *history) currentABEpoch(port uint16) int64 {
	return h.abEpoch[port]
}

func (h *history) ensureABEpoch(p abPortConfig, reason string) (int64, error) {
	var e abEpoch
	err := h.db.QueryRow(`SELECT id,port,started_ts,COALESCE(ended_ts,0),canary_percent,rate_mbps,gain,code_version,reason
		FROM ab_epochs WHERE port=? AND ended_ts IS NULL ORDER BY id DESC LIMIT 1`, p.Port).
		Scan(&e.ID, &e.Port, &e.Started, &e.Ended, &e.CanaryPercent, &e.RateMbps, &e.Gain, &e.CodeVersion, &e.Reason)
	if err == nil && e.CanaryPercent == p.CanaryPercent && math.Abs(e.RateMbps-p.RateMbps) < 0.0001 && e.Gain == p.Gain && e.CodeVersion == version {
		h.abEpoch[p.Port] = e.ID
		return e.ID, nil
	}
	if err != nil && err != sql.ErrNoRows {
		return 0, err
	}
	return h.beginABEpoch(p, reason)
}

func (h *history) beginABEpoch(p abPortConfig, reason string) (int64, error) {
	plan, err := effectiveABExperimentPlan(p.AnalysisPlan)
	if err != nil { return 0, err }
	now := time.Now().Unix()
	tx, err := h.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if _, err = tx.Exec("UPDATE ab_epochs SET ended_ts=? WHERE port=? AND ended_ts IS NULL", now, p.Port); err != nil {
		return 0, err
	}
	res, err := tx.Exec(`INSERT INTO ab_epochs(port,started_ts,canary_percent,rate_mbps,gain,code_version,reason)
		VALUES(?,?,?,?,?,?,?)`, p.Port, now, p.CanaryPercent, p.RateMbps, p.Gain, version, reason)
	if err != nil {
		return 0, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	if _, err = tx.Exec(`INSERT INTO ab_epoch_plans(epoch_id,alpha,power,expected_app_success_percent,app_success_ni_margin_pp,max_retrans_delta_pp,max_mean_rtt_delta_percent,min_goodput_delta_percent,bootstrap_block_minutes,predeclared)
		VALUES(?,?,?,?,?,?,?,?,?,1)`, id, plan.Alpha, plan.Power, plan.ExpectedAppSuccessPercent, plan.AppSuccessNIMarginPP, plan.MaxRetransDeltaPP, plan.MaxMeanRTTDeltaPercent, plan.MinGoodputDeltaPercent, plan.BootstrapBlockMinutes); err != nil { return 0, err }
	if err = tx.Commit(); err != nil {
		return 0, err
	}
	h.abEpoch[p.Port] = id
	h.addEvent("ab_epoch", map[string]any{"epoch_id": id, "port": p.Port, "canary_percent": p.CanaryPercent, "reason": reason})
	return id, nil
}

func (h *history) closeABEpoch(port uint16, reason string) error {
	now := time.Now().Unix()
	_, err := h.db.Exec("UPDATE ab_epochs SET ended_ts=? WHERE port=? AND ended_ts IS NULL", now, port)
	if err == nil {
		delete(h.abEpoch, port)
		h.addEvent("ab_epoch_close", map[string]any{"port": port, "reason": reason})
	}
	return err
}

func (h *history) seedABCohort(port uint16, cohort string, x portState, ts int64) error {
	row := abCohortSample{Time: ts, Port: port, Cohort: cohort, Group: x.Group, Sent: x.Sent, Acked: x.Acked, Retrans: x.Retrans, RTTSum: x.RTTSum, RTTSamples: x.RTTSamples}
	h.abLast[abCohortKey(port, cohort, x.Group)] = row
	_, err := h.db.Exec(`INSERT INTO ab_checkpoints(port,cohort,group_id,ts,sent,acked,retrans,rtt_sum,rtt_samples)
		VALUES(?,?,?,?,?,?,?,?,?) ON CONFLICT(port,cohort,group_id) DO UPDATE SET
		ts=excluded.ts,sent=excluded.sent,acked=excluded.acked,retrans=excluded.retrans,rtt_sum=excluded.rtt_sum,rtt_samples=excluded.rtt_samples`,
		port, cohort, x.Group, ts, x.Sent, x.Acked, x.Retrans, x.RTTSum, x.RTTSamples)
	return err
}

func (h *history) seedABSelector(port uint16, c selectorCount) error {
	h.abSelectorLast[port] = c
	_, err := h.db.Exec(`INSERT INTO ab_selector_checkpoints(port,baseline,canary,failure) VALUES(?,?,?,?)
		ON CONFLICT(port) DO UPDATE SET baseline=excluded.baseline,canary=excluded.canary,failure=excluded.failure`,
		port, c.Baseline, c.Canary, c.Failure)
	return err
}

func (h *history) seedAB(port uint16, baseline, canary portState, c selectorCount) error {
	ts := time.Now().Unix()
	if err := h.seedABCohort(port, "baseline", baseline, ts); err != nil {
		return err
	}
	if err := h.seedABCohort(port, "canary", canary, ts); err != nil {
		return err
	}
	return h.seedABSelector(port, c)
}

func (h *history) recordABCohort(epochID int64, cohort string, now portState, ts int64) error {
	key := abCohortKey(now.Port, cohort, now.Group)
	prev, ok := h.abLast[key]
	row := abCohortSample{Time: ts, EpochID: epochID, Port: now.Port, Cohort: cohort, Group: now.Group,
		Sent: now.Sent, Acked: now.Acked, Retrans: now.Retrans, Members: now.Members,
		RTTSum: now.RTTSum, RTTSamples: now.RTTSamples, RTTMax: now.RTTMax}
	row.Gap = !ok
	if ok {
		dt := ts - prev.Time
		if dt <= 0 || dt > 60 {
			row.Gap = true
		} else {
			row.Duration = uint64(dt)
			row.MemberSeconds = uint64(now.Members) * uint64(dt)
		}
		var reset bool
		row.Sent, reset = delta(now.Sent, prev.Sent); row.Gap = row.Gap || reset
		row.Acked, reset = delta(now.Acked, prev.Acked); row.Gap = row.Gap || reset
		row.Retrans, reset = delta(now.Retrans, prev.Retrans); row.Gap = row.Gap || reset
		row.RTTSum, reset = delta(now.RTTSum, prev.RTTSum); row.Gap = row.Gap || reset
		row.RTTSamples, reset = delta(now.RTTSamples, prev.RTTSamples); row.Gap = row.Gap || reset
	}
	tx, err := h.db.Begin()
	if err != nil { return err }
	defer tx.Rollback()
	for _, tier := range []struct{name string; seconds int64}{{"raw",10},{"minute",60},{"hour",3600}} {
		bucket := ts/tier.seconds*tier.seconds
		_, err = tx.Exec(`INSERT INTO ab_samples(tier,ts,epoch_id,port,cohort,group_id,duration_sec,member_seconds,sent,acked,retrans,members,rtt_sum,rtt_samples,rtt_max,gap)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
			ON CONFLICT(tier,ts,epoch_id,cohort,group_id) DO UPDATE SET
			duration_sec=duration_sec+excluded.duration_sec,member_seconds=member_seconds+excluded.member_seconds,
			sent=sent+excluded.sent,acked=acked+excluded.acked,retrans=retrans+excluded.retrans,
			members=excluded.members,rtt_sum=rtt_sum+excluded.rtt_sum,rtt_samples=rtt_samples+excluded.rtt_samples,
			rtt_max=max(rtt_max,excluded.rtt_max),gap=max(gap,excluded.gap)`,
			tier.name,bucket,epochID,now.Port,cohort,now.Group,row.Duration,row.MemberSeconds,row.Sent,row.Acked,row.Retrans,row.Members,row.RTTSum,row.RTTSamples,row.RTTMax,row.Gap)
		if err != nil { return err }
	}
	_, err = tx.Exec(`INSERT INTO ab_checkpoints(port,cohort,group_id,ts,sent,acked,retrans,rtt_sum,rtt_samples)
		VALUES(?,?,?,?,?,?,?,?,?) ON CONFLICT(port,cohort,group_id) DO UPDATE SET
		ts=excluded.ts,sent=excluded.sent,acked=excluded.acked,retrans=excluded.retrans,rtt_sum=excluded.rtt_sum,rtt_samples=excluded.rtt_samples`,
		now.Port,cohort,now.Group,ts,now.Sent,now.Acked,now.Retrans,now.RTTSum,now.RTTSamples)
	if err != nil { return err }
	if err = tx.Commit(); err != nil { return err }
	h.abLast[key] = abCohortSample{Time:ts,Port:now.Port,Cohort:cohort,Group:now.Group,Sent:now.Sent,Acked:now.Acked,Retrans:now.Retrans,RTTSum:now.RTTSum,RTTSamples:now.RTTSamples}
	return nil
}

func (h *history) recordABSelector(epochID int64, port uint16, now selectorCount, ts int64) error {
	prev, ok := h.abSelectorLast[port]
	row := abSelectorSample{Time: ts, EpochID: epochID, Port: port, Gap: !ok}
	if ok {
		var reset bool
		row.Baseline, reset = delta(now.Baseline, prev.Baseline); row.Gap = row.Gap || reset
		row.Canary, reset = delta(now.Canary, prev.Canary); row.Gap = row.Gap || reset
		row.Failure, reset = delta(now.Failure, prev.Failure); row.Gap = row.Gap || reset
	}
	tx, err := h.db.Begin()
	if err != nil { return err }
	defer tx.Rollback()
	for _, tier := range []struct{name string; seconds int64}{{"raw",10},{"minute",60},{"hour",3600}} {
		bucket := ts/tier.seconds*tier.seconds
		_, err = tx.Exec(`INSERT INTO ab_selector_samples(tier,ts,epoch_id,port,baseline,canary,failure,gap)
			VALUES(?,?,?,?,?,?,?,?) ON CONFLICT(tier,ts,epoch_id,port) DO UPDATE SET
			baseline=baseline+excluded.baseline,canary=canary+excluded.canary,failure=failure+excluded.failure,gap=max(gap,excluded.gap)`,
			tier.name,bucket,epochID,port,row.Baseline,row.Canary,row.Failure,row.Gap)
		if err != nil { return err }
	}
	_, err = tx.Exec(`INSERT INTO ab_selector_checkpoints(port,baseline,canary,failure) VALUES(?,?,?,?)
		ON CONFLICT(port) DO UPDATE SET baseline=excluded.baseline,canary=excluded.canary,failure=excluded.failure`,
		port,now.Baseline,now.Canary,now.Failure)
	if err != nil { return err }
	if err = tx.Commit(); err != nil { return err }
	h.abSelectorLast[port] = now
	return nil
}

func (h *history) recordABApp(x abAppSample) error {
	if x.Cohort != "baseline" && x.Cohort != "canary" {
		return fmt.Errorf("invalid cohort")
	}
	if x.Source == "" || len(x.Source) > 64 {
		return fmt.Errorf("invalid source")
	}
	if x.EpochID == 0 {
		x.EpochID = h.currentABEpoch(x.Port)
	}
	if x.EpochID == 0 {
		return fmt.Errorf("no active A/B epoch for port %d", x.Port)
	}
	if x.Time == 0 {
		x.Time = time.Now().Unix()
	}
	tx, err := h.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, tier := range []struct{name string; seconds int64}{{"raw",10},{"minute",60},{"hour",3600}} {
		bucket := x.Time/tier.seconds*tier.seconds
		_, err = tx.Exec(`INSERT INTO ab_app_samples(tier,ts,epoch_id,port,cohort,source,requests,success,errors,latency_sum_us,latency_samples,latency_max_us)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(tier,ts,epoch_id,cohort,source) DO UPDATE SET
			requests=requests+excluded.requests,success=success+excluded.success,errors=errors+excluded.errors,
			latency_sum_us=latency_sum_us+excluded.latency_sum_us,latency_samples=latency_samples+excluded.latency_samples,
			latency_max_us=max(latency_max_us,excluded.latency_max_us)`,
			tier.name,bucket,x.EpochID,x.Port,x.Cohort,x.Source,x.Requests,x.Success,x.Errors,x.LatencySumUS,x.LatencySamples,x.LatencyMaxUS)
		if err != nil { return err }
	}
	return tx.Commit()
}

func (h *history) abPlans(port uint16, from, to int64) (map[int64]abStoredPlan, error) {
	rows, err := h.db.Query(`SELECT p.epoch_id,p.alpha,p.power,p.expected_app_success_percent,p.app_success_ni_margin_pp,p.max_retrans_delta_pp,p.max_mean_rtt_delta_percent,p.min_goodput_delta_percent,p.bootstrap_block_minutes,p.predeclared
		FROM ab_epoch_plans p JOIN ab_epochs e ON e.id=p.epoch_id
		WHERE e.port=? AND e.started_ts<? AND COALESCE(e.ended_ts,?)>=?`,port,to,to,from)
	if err != nil { return nil, err }
	defer rows.Close(); out:=map[int64]abStoredPlan{}
	for rows.Next(){var id int64;var p abExperimentPlan;var pre int;if err:=rows.Scan(&id,&p.Alpha,&p.Power,&p.ExpectedAppSuccessPercent,&p.AppSuccessNIMarginPP,&p.MaxRetransDeltaPP,&p.MaxMeanRTTDeltaPercent,&p.MinGoodputDeltaPercent,&p.BootstrapBlockMinutes,&pre);err!=nil{return nil,err};out[id]=abStoredPlan{Plan:p,Predeclared:pre!=0}}
	return out,rows.Err()
}

func (h *history) abPorts() ([]uint16, error) {
	rows, err := h.db.Query("SELECT DISTINCT port FROM ab_epochs ORDER BY port")
	if err != nil { return nil, err }
	defer rows.Close()
	var out []uint16
	for rows.Next() {
		var port uint16
		if err := rows.Scan(&port); err != nil { return nil, err }
		out = append(out, port)
	}
	return out, rows.Err()
}

func (h *history) abEpochs(port uint16, from, to int64) ([]abEpoch, error) {
	rows, err := h.db.Query(`SELECT id,port,started_ts,COALESCE(ended_ts,0),canary_percent,rate_mbps,gain,code_version,reason
		FROM ab_epochs WHERE port=? AND started_ts<? AND COALESCE(ended_ts,?)>=? ORDER BY id`,port,to,to,from)
	if err != nil { return nil, err }
	defer rows.Close()
	var out []abEpoch
	for rows.Next() {
		var x abEpoch
		if err := rows.Scan(&x.ID,&x.Port,&x.Started,&x.Ended,&x.CanaryPercent,&x.RateMbps,&x.Gain,&x.CodeVersion,&x.Reason); err != nil { return nil, err }
		out=append(out,x)
	}
	return out,rows.Err()
}

func (h *history) abSamples(tier string, port uint16, from, to int64) ([]abCohortSample, error) {
	if tier!="raw" && tier!="minute" && tier!="hour" { return nil,fmt.Errorf("invalid tier") }
	rows,err:=h.db.Query(`SELECT ts,epoch_id,port,cohort,group_id,duration_sec,member_seconds,sent,acked,retrans,members,rtt_sum,rtt_samples,rtt_max,gap
		FROM ab_samples WHERE tier=? AND port=? AND ts>=? AND ts<? ORDER BY ts,epoch_id,cohort`,tier,port,from,to)
	if err!=nil{return nil,err}; defer rows.Close()
	var out []abCohortSample
	for rows.Next(){var x abCohortSample; if err:=rows.Scan(&x.Time,&x.EpochID,&x.Port,&x.Cohort,&x.Group,&x.Duration,&x.MemberSeconds,&x.Sent,&x.Acked,&x.Retrans,&x.Members,&x.RTTSum,&x.RTTSamples,&x.RTTMax,&x.Gap);err!=nil{return nil,err};out=append(out,x)}
	return out,rows.Err()
}

func (h *history) abSelectorSamples(tier string, port uint16, from, to int64) ([]abSelectorSample,error){
	if tier!="raw"&&tier!="minute"&&tier!="hour"{return nil,fmt.Errorf("invalid tier")}
	rows,err:=h.db.Query(`SELECT ts,epoch_id,port,baseline,canary,failure,gap FROM ab_selector_samples WHERE tier=? AND port=? AND ts>=? AND ts<? ORDER BY ts,epoch_id`,tier,port,from,to)
	if err!=nil{return nil,err};defer rows.Close();var out []abSelectorSample
	for rows.Next(){var x abSelectorSample;if err:=rows.Scan(&x.Time,&x.EpochID,&x.Port,&x.Baseline,&x.Canary,&x.Failure,&x.Gap);err!=nil{return nil,err};out=append(out,x)}
	return out,rows.Err()
}

func (h *history) abAppSamples(tier string, port uint16, from, to int64) ([]abAppSample, error) {
	if tier != "raw" && tier != "minute" && tier != "hour" {
		return nil, fmt.Errorf("invalid tier")
	}
	rows, err := h.db.Query(`SELECT ts,epoch_id,port,cohort,source,requests,success,errors,latency_sum_us,latency_samples,latency_max_us
		FROM ab_app_samples WHERE tier=? AND port=? AND ts>=? AND ts<? ORDER BY ts,epoch_id,cohort,source`, tier, port, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []abAppSample
	for rows.Next() {
		var x abAppSample
		if err := rows.Scan(&x.Time, &x.EpochID, &x.Port, &x.Cohort, &x.Source, &x.Requests, &x.Success, &x.Errors, &x.LatencySumUS, &x.LatencySamples, &x.LatencyMaxUS); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

func (h *history) abSummaries(port uint16, from,to int64) ([]abSummary,error){
	rows,err:=h.db.Query(`SELECT s.epoch_id,s.port,s.cohort,e.canary_percent,
		SUM(s.duration_sec),SUM(s.member_seconds),SUM(s.sent),SUM(s.acked),SUM(s.retrans),
		SUM(s.rtt_sum),SUM(s.rtt_samples),MAX(s.rtt_max),SUM(CASE WHEN s.gap!=0 THEN 1 ELSE 0 END)
		FROM ab_samples s JOIN ab_epochs e ON e.id=s.epoch_id
		WHERE s.tier='minute' AND s.port=? AND s.ts>=? AND s.ts<?
		GROUP BY s.epoch_id,s.port,s.cohort,e.canary_percent ORDER BY s.epoch_id,s.cohort`,port,from,to)
	if err!=nil{return nil,err};defer rows.Close()
	var out []abSummary
	for rows.Next(){var x abSummary;var rttSum uint64;if err:=rows.Scan(&x.EpochID,&x.Port,&x.Cohort,&x.CanaryPercent,&x.DurationSeconds,&x.MemberSeconds,&x.SentBytes,&x.AckedBytes,&x.RetransBytes,&rttSum,&x.RTTSamples,&x.MaxRTTMS,&x.GapSamples);err!=nil{return nil,err};x.MaxRTTMS/=1000;if x.SentBytes>0{x.RetransPercent=100*float64(x.RetransBytes)/float64(x.SentBytes)};if x.DurationSeconds>0{x.GoodputMbps=float64(x.AckedBytes)*8/float64(x.DurationSeconds)/1e6};if x.MemberSeconds>0{x.GoodputPerMemberMbps=float64(x.AckedBytes)*8/float64(x.MemberSeconds)/1e6};if x.RTTSamples>0{x.MeanRTTMS=float64(rttSum)/float64(x.RTTSamples)/1000};out=append(out,x)}
	if err:=rows.Err();err!=nil{return nil,err}
	assign:=map[string]uint64{};fail:=map[int64]uint64{}
	sr,err:=h.db.Query(`SELECT epoch_id,SUM(baseline),SUM(canary),SUM(failure) FROM ab_selector_samples WHERE tier='minute' AND port=? AND ts>=? AND ts<? GROUP BY epoch_id`,port,from,to)
	if err!=nil{return nil,err}
	for sr.Next(){var eid int64;var b,c,f uint64;if err:=sr.Scan(&eid,&b,&c,&f);err!=nil{sr.Close();return nil,err};assign[fmt.Sprintf("%d/baseline",eid)]=b;assign[fmt.Sprintf("%d/canary",eid)]=c;fail[eid]=f};sr.Close()
	app:=map[string]abSummary{}
	ar,err:=h.db.Query(`SELECT epoch_id,cohort,SUM(requests),SUM(success),SUM(errors),SUM(latency_sum_us),SUM(latency_samples),MAX(latency_max_us)
		FROM ab_app_samples WHERE tier='minute' AND port=? AND ts>=? AND ts<? GROUP BY epoch_id,cohort`,port,from,to)
	if err!=nil{return nil,err}
	for ar.Next(){var eid int64;var cohort string;var x abSummary;var lsum,lsamples,lmax uint64;if err:=ar.Scan(&eid,&cohort,&x.AppRequests,&x.AppSuccess,&x.AppErrors,&lsum,&lsamples,&lmax);err!=nil{ar.Close();return nil,err};if x.AppRequests>0{x.AppSuccessPercent=100*float64(x.AppSuccess)/float64(x.AppRequests)};if lsamples>0{x.AppMeanLatencyMS=float64(lsum)/float64(lsamples)/1000};x.AppMaxLatencyMS=float64(lmax)/1000;app[fmt.Sprintf("%d/%s",eid,cohort)]=x};ar.Close()
	for i:=range out{key:=fmt.Sprintf("%d/%s",out[i].EpochID,out[i].Cohort);out[i].AssignedConnections=assign[key];out[i].SelectorFailures=fail[out[i].EpochID];a:=app[key];out[i].AppRequests=a.AppRequests;out[i].AppSuccess=a.AppSuccess;out[i].AppErrors=a.AppErrors;out[i].AppSuccessPercent=a.AppSuccessPercent;out[i].AppMeanLatencyMS=a.AppMeanLatencyMS;out[i].AppMaxLatencyMS=a.AppMaxLatencyMS}
	return out,nil
}

func (h *history) pruneAB() error {
	now:=time.Now().Unix()
	for _,x:=range []struct{tier string;cutoff int64}{{"raw",30*86400},{"minute",365*86400},{"hour",3*365*86400}}{
		if _,err:=h.db.Exec("DELETE FROM ab_samples WHERE tier=? AND ts<?",x.tier,now-x.cutoff);err!=nil{return err}
		if _,err:=h.db.Exec("DELETE FROM ab_selector_samples WHERE tier=? AND ts<?",x.tier,now-x.cutoff);err!=nil{return err}
		if _,err:=h.db.Exec("DELETE FROM ab_app_samples WHERE tier=? AND ts<?",x.tier,now-x.cutoff);err!=nil{return err}
	}
	return nil
}

func marshalABManifest(port uint16, from,to int64, tier string, epochs []abEpoch, summaries []abSummary) []byte {
	b,_:=json.MarshalIndent(map[string]any{
		"schema_version":1,"generated_at":time.Now().UTC().Format(time.RFC3339),"code_version":version,
		"port":port,"from":from,"to":to,"tier":tier,"epochs":epochs,"summaries":summaries,
		"notes":[]string{"gap=true intervals must be excluded from inferential analysis","percentage changes create separate epochs","application metrics are optional and source-labelled"},
	},"","  ")
	return b
}