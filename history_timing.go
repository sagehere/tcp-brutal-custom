package main

import (
	"database/sql"
	"strconv"
)

func csvRate(rate *float64) string {
	if rate == nil {
		return ""
	}
	return strconv.FormatFloat(*rate, 'f', 6, 64)
}

func migrateHistory(db *sql.DB) error {
	var version int
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	if version >= 1 {
		return nil
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, q := range []string{
		"ALTER TABLE samples ADD COLUMN start_ms INTEGER NOT NULL DEFAULT 0",
		"ALTER TABLE samples ADD COLUMN end_ms INTEGER NOT NULL DEFAULT 0",
		"ALTER TABLE samples ADD COLUMN duration_ms INTEGER NOT NULL DEFAULT 0",
		"ALTER TABLE samples ADD COLUMN timing_version INTEGER NOT NULL DEFAULT 0",
		"PRAGMA user_version=1",
	} {
		if _, err = tx.Exec(q); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Integer cumulative allocation preserves every byte at bucket boundaries.
func counterPart(total uint64, start, end, duration int64) uint64 {
	at := func(n int64) uint64 {
		return total/uint64(duration)*uint64(n) + total%uint64(duration)*uint64(n)/uint64(duration)
	}
	return at(end) - at(start)
}

func intervalPieces(row sample, seconds int64) []sample {
	if row.Gap || seconds == 10 {
		row.Time = row.Time / seconds * seconds
		if row.Gap {
			row.DurationMS = 0
		}
		return []sample{row}
	}
	out := []sample{}
	wallDuration := row.EndMS - row.StartMS
	for start := row.StartMS; start < row.EndMS; {
		bucket := start / (seconds * 1000) * (seconds * 1000)
		end := min(row.EndMS, bucket+seconds*1000)
		x := row
		x.Time, x.StartMS, x.EndMS = bucket/1000, start, end
		x.DurationMS = int64(counterPart(uint64(row.DurationMS), start-row.StartMS, end-row.StartMS, wallDuration))
		split := func(n uint64) uint64 { return counterPart(n, start-row.StartMS, end-row.StartMS, wallDuration) }
		x.Sent, x.Acked, x.Retrans = split(row.Sent), split(row.Acked), split(row.Retrans)
		x.Success, x.Failure = split(row.Success), split(row.Failure)
		x.RTTSum, x.RTTSamples = split(row.RTTSum), split(row.RTTSamples)
		out = append(out, x)
		start = end
	}
	return out
}
