package suite

import (
	"database/sql"
	"time"
)

// LedgerRetention keeps finished quota periods for a year.
const LedgerRetention = 365 * 24 * time.Hour

// StoredLedger is a ledger row with its account.
type StoredLedger struct {
	AuthIndex string `json:"auth_index"`
	Provider  string `json:"provider"`
	LedgerCycle
}

// PutLedger upserts finished periods (a later computation of the same period
// replaces the earlier one) and prunes rows older than a year.
func (c *Controller) PutLedger(authIndex, provider string, cycles []LedgerCycle) error {
	if len(cycles) == 0 {
		return nil
	}
	return c.withExt(func(db *sql.DB) error {
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		defer tx.Rollback()
		for _, e := range cycles {
			if _, err := tx.Exec(`INSERT OR REPLACE INTO forecast_ledger(auth_index, provider, window, start, end, early, peak, usd, tokens, requests, capacity_usd, capacity_tokens, samples, confidence)
				VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, authIndex, provider, e.Window, e.Start.Unix(), e.End.Unix(), b2i(e.Early), e.PeakPercent, e.USD, e.Tokens, e.Requests,
				nullable(e.CapacityUSD), nullable(e.CapacityTokens), e.Samples, e.Confidence); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(`DELETE FROM forecast_ledger WHERE end < ?`, c.now().Add(-LedgerRetention).Unix()); err != nil {
			return err
		}
		return tx.Commit()
	})
}

func nullable(v *float64) any {
	if v == nil {
		return nil
	}
	return *v
}

// Ledger lists finished periods since a time, newest first ("" = all accounts).
func (c *Controller) Ledger(authIndex string, since time.Time) ([]StoredLedger, error) {
	var out []StoredLedger
	err := c.withExt(func(db *sql.DB) error {
		rows, err := db.Query(`SELECT auth_index, provider, window, start, end, early, peak, usd, tokens, requests, capacity_usd, capacity_tokens, samples, confidence
			FROM forecast_ledger WHERE (? = '' OR auth_index = ?) AND end >= ? ORDER BY end DESC`, authIndex, authIndex, since.Unix())
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var e StoredLedger
			var start, end int64
			var early int
			var cu, ct sql.NullFloat64
			if err := rows.Scan(&e.AuthIndex, &e.Provider, &e.Window, &start, &end, &early, &e.PeakPercent, &e.USD, &e.Tokens, &e.Requests, &cu, &ct, &e.Samples, &e.Confidence); err != nil {
				return err
			}
			e.Start, e.End, e.Early = time.Unix(start, 0).UTC(), time.Unix(end, 0).UTC(), early == 1
			if cu.Valid {
				e.CapacityUSD = ptr(cu.Float64)
			}
			if ct.Valid {
				e.CapacityTokens = ptr(ct.Float64)
			}
			out = append(out, e)
		}
		return rows.Err()
	})
	return out, err
}

// CandyRun is one manual candy test (0.6, module candy_test).
type CandyRun struct {
	ID            int64     `json:"id"`
	AuthIndex     string    `json:"auth_index"`
	Provider      string    `json:"provider"`
	Model         string    `json:"model"`
	Effort        string    `json:"effort,omitempty"`
	StartedAt     time.Time `json:"started_at"`
	FinishedAt    time.Time `json:"finished_at,omitempty"`
	Status        string    `json:"status"` // running | done | error | interrupted
	Passed        bool      `json:"passed"`
	Answer        string    `json:"answer,omitempty"`
	Error         string    `json:"error,omitempty"`
	InputTokens   int64     `json:"input_tokens"`
	OutputTokens  int64     `json:"output_tokens"`
	Reasoning     int64     `json:"reasoning_tokens"`
	LatencyMS     int64     `json:"latency_ms"`
	ResponseModel string    `json:"response_model,omitempty"`
}

// CandyKeep is how many runs per account are kept.
const CandyKeep = 50

// StartCandyRun inserts a running record and returns its id.
func (c *Controller) StartCandyRun(r CandyRun) (int64, error) {
	var id int64
	err := c.withExt(func(db *sql.DB) error {
		res, err := db.Exec(`INSERT INTO candy_runs(auth_index, provider, model, effort, started_at, status) VALUES(?,?,?,?,?, 'running')`,
			r.AuthIndex, r.Provider, r.Model, r.Effort, r.StartedAt.UnixNano())
		if err != nil {
			return err
		}
		id, err = res.LastInsertId()
		return err
	})
	return id, err
}

// FinishCandyRun stores the outcome and keeps the newest CandyKeep runs per account.
func (c *Controller) FinishCandyRun(r CandyRun) error {
	return c.withExt(func(db *sql.DB) error {
		if _, err := db.Exec(`UPDATE candy_runs SET finished_at=?, status=?, passed=?, answer=?, error=?, input_tokens=?, output_tokens=?, reasoning_tokens=?, latency_ms=?, response_model=? WHERE id=?`,
			r.FinishedAt.UnixNano(), r.Status, b2i(r.Passed), r.Answer, r.Error, r.InputTokens, r.OutputTokens, r.Reasoning, r.LatencyMS, r.ResponseModel, r.ID); err != nil {
			return err
		}
		_, err := db.Exec(`DELETE FROM candy_runs WHERE auth_index = ? AND id NOT IN (SELECT id FROM candy_runs WHERE auth_index = ? ORDER BY started_at DESC LIMIT ?)`, r.AuthIndex, r.AuthIndex, CandyKeep)
		return err
	})
}

// InterruptCandyRuns marks runs still "running" that started before a time
// (left behind by a restart; a live run always finishes within its timeout).
func (c *Controller) InterruptCandyRuns(before time.Time) error {
	return c.withExt(func(db *sql.DB) error {
		_, err := db.Exec(`UPDATE candy_runs SET status='interrupted', finished_at=? WHERE status='running' AND started_at < ?`, c.now().UnixNano(), before.UnixNano())
		return err
	})
}

// CandyRuns lists runs since a time, newest first ("" = all accounts).
func (c *Controller) CandyRuns(authIndex string, since time.Time, limit int) ([]CandyRun, error) {
	var out []CandyRun
	err := c.withExt(func(db *sql.DB) error {
		rows, err := db.Query(`SELECT id, auth_index, provider, model, effort, started_at, finished_at, status, passed, answer, error, input_tokens, output_tokens, reasoning_tokens, latency_ms, response_model
			FROM candy_runs WHERE (? = '' OR auth_index = ?) AND started_at >= ? ORDER BY started_at DESC LIMIT ?`, authIndex, authIndex, since.UnixNano(), limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r CandyRun
			var started, finished int64
			var passed int
			if err := rows.Scan(&r.ID, &r.AuthIndex, &r.Provider, &r.Model, &r.Effort, &started, &finished, &r.Status, &passed, &r.Answer, &r.Error,
				&r.InputTokens, &r.OutputTokens, &r.Reasoning, &r.LatencyMS, &r.ResponseModel); err != nil {
				return err
			}
			r.StartedAt, r.Passed = time.Unix(0, started).UTC(), passed == 1
			if finished > 0 {
				r.FinishedAt = time.Unix(0, finished).UTC()
			}
			out = append(out, r)
		}
		return rows.Err()
	})
	return out, err
}
