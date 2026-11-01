package suite

import (
	"database/sql"
	"time"
)

// Monitor samples are buffered in memory and written in batches so the
// usage.handle hot path never waits for SQLite.
const monitorFlushSize = 200

// ObserveSample queues one sample (never blocks on disk).
func (c *Controller) ObserveSample(s MonitorSample) {
	c.monMu.Lock()
	c.monBuf = append(c.monBuf, s)
	full := len(c.monBuf) >= monitorFlushSize
	if c.monTimer == nil && !full {
		c.monTimer = time.AfterFunc(2*time.Second, func() { _ = c.FlushSamples() })
	}
	c.monMu.Unlock()
	if full {
		go func() { _ = c.FlushSamples() }()
	}
}

// FlushSamples writes queued samples and prunes rows past retention.
func (c *Controller) FlushSamples() error {
	c.monMu.Lock()
	buf := c.monBuf
	c.monBuf = nil
	if c.monTimer != nil {
		c.monTimer.Stop()
		c.monTimer = nil
	}
	c.monMu.Unlock()
	if len(buf) == 0 {
		return nil
	}
	return c.withExt(func(db *sql.DB) error {
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		stmt, err := tx.Prepare(`INSERT INTO monitor_samples(at, auth_index, provider, model, response_model, effort, stream, failed, status, err_kind, output, reasoning, latency_ms, ttft_ms, input)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`)
		if err != nil {
			tx.Rollback()
			return err
		}
		for _, s := range buf {
			if _, err := stmt.Exec(s.At.UnixNano(), s.AuthIndex, s.Provider, s.Model, s.ResponseModel, s.Effort, b2i(s.Stream), b2i(s.Failed), s.Status, s.ErrKind, s.Output, s.Reasoning, s.LatencyMS, s.TTFTMS, s.Input); err != nil {
				stmt.Close()
				tx.Rollback()
				return err
			}
		}
		stmt.Close()
		if _, err := tx.Exec(`DELETE FROM monitor_samples WHERE at < ?`, c.clock().Add(-days(c.retentionCached().MonitorDays)).UnixNano()); err != nil {
			tx.Rollback()
			return err
		}
		return tx.Commit()
	})
}

// MonitorSamples returns samples at or after since (after flushing the queue).
func (c *Controller) MonitorSamples(since time.Time) ([]MonitorSample, error) {
	_ = c.FlushSamples()
	var out []MonitorSample
	err := c.withExt(func(db *sql.DB) error {
		rows, err := db.Query(`SELECT at, auth_index, provider, model, response_model, effort, stream, failed, status, err_kind, output, reasoning, latency_ms, ttft_ms, input
			FROM monitor_samples WHERE at >= ? ORDER BY at`, since.UnixNano())
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var s MonitorSample
			var at int64
			var stream, failed int
			if err := rows.Scan(&at, &s.AuthIndex, &s.Provider, &s.Model, &s.ResponseModel, &s.Effort, &stream, &failed, &s.Status, &s.ErrKind, &s.Output, &s.Reasoning, &s.LatencyMS, &s.TTFTMS, &s.Input); err != nil {
				return err
			}
			s.At, s.Stream, s.Failed = time.Unix(0, at).UTC(), stream == 1, failed == 1
			out = append(out, s)
		}
		return rows.Err()
	})
	return out, err
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// clock is the controller's injectable clock (time.Now when unset), so the
// 14-day retention follows the same time source as the samples.
func (c *Controller) clock() time.Time {
	if c != nil && c.now != nil {
		return c.now()
	}
	return time.Now()
}
