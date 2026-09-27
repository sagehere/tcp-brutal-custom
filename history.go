package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

type sample struct {
	Time       int64  `json:"time"`
	Port       uint16 `json:"port"`
	Group      uint64 `json:"group"`
	Sent       uint64 `json:"sent"`
	Acked      uint64 `json:"acked"`
	Retrans    uint64 `json:"retrans"`
	Expected   uint64 `json:"expected_bytes"`
	Actual     uint64 `json:"actual_bytes"`
	Success    uint64 `json:"success"`
	Failure    uint64 `json:"failure"`
	Members    uint32 `json:"members"`
	RTTSum     uint64 `json:"rtt_sum_us"`
	RTTSamples uint64 `json:"rtt_samples"`
	RTTMax     uint32 `json:"rtt_max_us"`
	Gap        bool   `json:"gap"`
}

func sendBytes(sent, retrans uint64) (uint64, uint64) {
	if retrans >= sent {
		return 0, sent
	}
	return sent - retrans, sent
}

type event struct {
	Time   int64  `json:"time"`
	Kind   string `json:"kind"`
	Detail string `json:"detail"`
}

type history struct {
	db   *sql.DB
	dir  string
	last map[string]sample
}

func openHistory() (*history, error) {
	return openHistoryAt(dataDir)
}

func openHistoryAt(dir string) (*history, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", filepath.Join(dir, "history.db"))
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	for _, q := range []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA busy_timeout=3000",
		`CREATE TABLE IF NOT EXISTS samples (tier TEXT NOT NULL, ts INTEGER NOT NULL, port INTEGER NOT NULL, group_id INTEGER NOT NULL, sent INTEGER NOT NULL, acked INTEGER NOT NULL, retrans INTEGER NOT NULL, success INTEGER NOT NULL, failure INTEGER NOT NULL, members INTEGER NOT NULL, rtt_sum INTEGER NOT NULL, rtt_samples INTEGER NOT NULL, rtt_max INTEGER NOT NULL, gap INTEGER NOT NULL, PRIMARY KEY(tier,ts,port,group_id))`,
		`CREATE TABLE IF NOT EXISTS events (ts INTEGER NOT NULL, kind TEXT NOT NULL, detail TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS checkpoints (port INTEGER NOT NULL, group_id INTEGER NOT NULL, sent INTEGER NOT NULL, acked INTEGER NOT NULL, retrans INTEGER NOT NULL, success INTEGER NOT NULL, failure INTEGER NOT NULL, rtt_sum INTEGER NOT NULL, rtt_samples INTEGER NOT NULL, PRIMARY KEY(port,group_id))`,
	} {
		if _, err = db.Exec(q); err != nil {
			db.Close()
			return nil, err
		}
	}
	h := &history{db: db, dir: dir, last: map[string]sample{}}
	rows, err := db.Query("SELECT port,group_id,sent,acked,retrans,success,failure,rtt_sum,rtt_samples FROM checkpoints")
	if err != nil {
		db.Close()
		return nil, err
	}
	for rows.Next() {
		var x sample
		if err = rows.Scan(&x.Port, &x.Group, &x.Sent, &x.Acked, &x.Retrans, &x.Success, &x.Failure, &x.RTTSum, &x.RTTSamples); err != nil {
			rows.Close()
			db.Close()
			return nil, err
		}
		h.last[fmt.Sprintf("%d/%d", x.Port, x.Group)] = x
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		db.Close()
		return nil, err
	}
	return h, nil
}

func (h *history) close() { h.db.Close() }

func delta(now, prev uint64) (uint64, bool) {
	if now < prev {
		return 0, true
	}
	return now - prev, false
}

func (h *history) record(now sample) error {
	key := fmt.Sprintf("%d/%d", now.Port, now.Group)
	prev, ok := h.last[key]
	row := now
	row.Gap = !ok
	if ok {
		var reset bool
		row.Sent, reset = delta(now.Sent, prev.Sent)
		row.Gap = row.Gap || reset
		row.Acked, reset = delta(now.Acked, prev.Acked)
		row.Gap = row.Gap || reset
		row.Retrans, reset = delta(now.Retrans, prev.Retrans)
		row.Gap = row.Gap || reset
		row.Success, reset = delta(now.Success, prev.Success)
		row.Gap = row.Gap || reset
		row.Failure, reset = delta(now.Failure, prev.Failure)
		row.Gap = row.Gap || reset
		row.RTTSum, reset = delta(now.RTTSum, prev.RTTSum)
		row.Gap = row.Gap || reset
		row.RTTSamples, reset = delta(now.RTTSamples, prev.RTTSamples)
		row.Gap = row.Gap || reset
	} else {
		// New groups start at zero. Keep their first interval, including short connections.
	}
	tx, err := h.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, tier := range []struct {
		name    string
		seconds int64
	}{{"raw", 10}, {"minute", 60}, {"hour", 3600}} {
		ts := now.Time / tier.seconds * tier.seconds
		_, err = tx.Exec(`INSERT INTO samples VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(tier,ts,port,group_id) DO UPDATE SET sent=sent+excluded.sent,acked=acked+excluded.acked,retrans=retrans+excluded.retrans,success=success+excluded.success,failure=failure+excluded.failure,members=excluded.members,rtt_sum=rtt_sum+excluded.rtt_sum,rtt_samples=rtt_samples+excluded.rtt_samples,rtt_max=max(rtt_max,excluded.rtt_max),gap=max(gap,excluded.gap)`, tier.name, ts, row.Port, row.Group, row.Sent, row.Acked, row.Retrans, row.Success, row.Failure, row.Members, row.RTTSum, row.RTTSamples, row.RTTMax, row.Gap)
		if err != nil {
			return err
		}
	}
	if _, err = tx.Exec(`INSERT INTO checkpoints VALUES(?,?,?,?,?,?,?,?,?) ON CONFLICT(port,group_id) DO UPDATE SET sent=excluded.sent,acked=excluded.acked,retrans=excluded.retrans,success=excluded.success,failure=excluded.failure,rtt_sum=excluded.rtt_sum,rtt_samples=excluded.rtt_samples`, now.Port, now.Group, now.Sent, now.Acked, now.Retrans, now.Success, now.Failure, now.RTTSum, now.RTTSamples); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	h.last[key] = now
	return nil
}

func (h *history) addEvent(kind string, detail any) {
	b, _ := json.Marshal(detail)
	h.db.Exec("INSERT INTO events VALUES(?,?,?)", time.Now().Unix(), kind, string(b))
}

func (h *history) prune() error {
	now := time.Now().Unix()
	for _, x := range []struct {
		tier   string
		cutoff int64
	}{{"raw", 7 * 86400}, {"minute", 90 * 86400}, {"hour", 365 * 86400}} {
		if _, err := h.db.Exec("DELETE FROM samples WHERE tier=? AND ts<?", x.tier, now-x.cutoff); err != nil {
			return err
		}
	}
	if _, err := h.db.Exec("DELETE FROM events WHERE ts<?", now-365*86400); err != nil {
		return err
	}
	path := filepath.Join(h.dir, "history.db")
	fi, err := os.Stat(path)
	if err != nil {
		return err
	}
	if fi.Size() > 1<<30 {
		h.addEvent("history_gap", map[string]string{"reason": "size limit"})
		if _, err := h.db.Exec("PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
			return err
		}
		if _, err := h.db.Exec("VACUUM"); err != nil {
			return err
		}
		fi, err = os.Stat(path)
		if err != nil {
			return err
		}
		for _, tier := range []string{"raw", "minute", "hour"} {
			for fi.Size() > 1<<30 {
				result, e := h.db.Exec("DELETE FROM samples WHERE rowid IN (SELECT rowid FROM samples WHERE tier=? ORDER BY ts LIMIT 100000)", tier)
				if e != nil {
					return e
				}
				removed, e := result.RowsAffected()
				if e != nil {
					return e
				}
				if removed == 0 {
					break
				}
				if _, e = h.db.Exec("PRAGMA wal_checkpoint(TRUNCATE)"); e != nil {
					return e
				}
				if _, e = h.db.Exec("VACUUM"); e != nil {
					return e
				}
				fi, e = os.Stat(path)
				if e != nil {
					return e
				}
			}
		}
		if fi.Size() > 1<<30 {
			return fmt.Errorf("history size limit could not be recovered")
		}
	}
	return nil
}

func (h *history) query(tier string, from, to int64, port uint16) ([]sample, error) {
	if tier != "raw" && tier != "minute" && tier != "hour" {
		return nil, fmt.Errorf("invalid tier")
	}
	if from >= to || to-from > 366*86400 {
		return nil, fmt.Errorf("invalid time range")
	}
	rows, err := h.db.Query(`SELECT ts,port,group_id,sent,acked,retrans,success,failure,members,rtt_sum,rtt_samples,rtt_max,gap FROM samples WHERE tier=? AND ts>=? AND ts<? AND (?=0 OR port=?) ORDER BY ts,port LIMIT 100000`, tier, from, to, port, port)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []sample{}
	for rows.Next() {
		var x sample
		if err := rows.Scan(&x.Time, &x.Port, &x.Group, &x.Sent, &x.Acked, &x.Retrans, &x.Success, &x.Failure, &x.Members, &x.RTTSum, &x.RTTSamples, &x.RTTMax, &x.Gap); err != nil {
			return nil, err
		}
		x.Expected, x.Actual = sendBytes(x.Sent, x.Retrans)
		out = append(out, x)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) == 100000 {
		return nil, fmt.Errorf("too many samples: select a port or coarser interval")
	}
	return out, nil
}

func (h *history) events(from, to int64) ([]event, error) {
	rows, err := h.db.Query("SELECT ts,kind,detail FROM events WHERE ts>=? AND ts<? ORDER BY ts LIMIT 10000", from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []event{}
	for rows.Next() {
		var x event
		if err := rows.Scan(&x.Time, &x.Kind, &x.Detail); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}
