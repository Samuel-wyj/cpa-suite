package suite

import (
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"time"
)

// QuotaSample is one official quota reading obtained from a provider's quota
// endpoint (stored in the extension DB, table quota_samples).
type QuotaSample struct {
	AuthIndex     string
	Provider      string
	Window        string
	WindowSeconds int64
	At            time.Time
	UsedPercent   float64
	ResetAt       time.Time
	Source        string
}

// QuotaFetch is the per-account cache / rate-limit record of the last call to
// an official quota endpoint.
type QuotaFetch struct {
	AuthIndex string
	Provider  string
	At        time.Time
	OK        bool
	Error     string
	Payload   string
}

// ModuleMode returns the effective mode of a module ("" when unknown).
func (c *Controller) ModuleMode(id string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, m := range c.modules {
		if m.ID == id {
			return m.Mode
		}
	}
	return ""
}

var errExtClosed = errors.New("extension database closed")

func (c *Controller) withExt(fn func(*sql.DB) error) error {
	c.extMu.Lock()
	defer c.extMu.Unlock()
	if c.ext == nil {
		return errExtClosed
	}
	return fn(c.ext.db)
}

// AddQuotaSamples stores readings (duplicates are ignored) and prunes readings
// older than 40 days.
func (c *Controller) AddQuotaSamples(samples []QuotaSample) error {
	ret := c.CurrentRetention()
	return c.withExt(func(db *sql.DB) error {
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		defer tx.Rollback()
		for _, s := range samples {
			reset := int64(0)
			if !s.ResetAt.IsZero() {
				reset = s.ResetAt.Unix()
			}
			if _, err := tx.Exec(`INSERT OR IGNORE INTO quota_samples(auth_index, provider, window, window_seconds, at, used_percent, reset_at, source)
				VALUES(?,?,?,?,?,?,?,?)`, s.AuthIndex, s.Provider, s.Window, s.WindowSeconds, s.At.UnixNano(), s.UsedPercent, reset, s.Source); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(`DELETE FROM quota_samples WHERE at < ?`, c.now().Add(-days(ret.QuotaDays)).UnixNano()); err != nil {
			return err
		}
		return tx.Commit()
	})
}

// QuotaSamples returns stored readings of one account since a time, oldest first.
func (c *Controller) QuotaSamples(authIndex string, since time.Time) ([]QuotaSample, error) {
	var out []QuotaSample
	err := c.withExt(func(db *sql.DB) error {
		rows, err := db.Query(`SELECT provider, window, window_seconds, at, used_percent, reset_at, source FROM quota_samples
			WHERE auth_index = ? AND at >= ? ORDER BY at`, authIndex, since.UnixNano())
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			s := QuotaSample{AuthIndex: authIndex}
			var at, reset int64
			if err := rows.Scan(&s.Provider, &s.Window, &s.WindowSeconds, &at, &s.UsedPercent, &reset, &s.Source); err != nil {
				return err
			}
			s.At = time.Unix(0, at).UTC()
			if reset > 0 {
				s.ResetAt = time.Unix(reset, 0).UTC()
			}
			out = append(out, s)
		}
		return rows.Err()
	})
	return out, err
}

// LastQuotaFetch returns the cache record for an account.
func (c *Controller) LastQuotaFetch(authIndex string) (QuotaFetch, bool, error) {
	f := QuotaFetch{AuthIndex: authIndex}
	found := false
	err := c.withExt(func(db *sql.DB) error {
		var at int64
		var ok int
		e := db.QueryRow(`SELECT provider, at, ok, error, payload FROM quota_fetch WHERE auth_index = ?`, authIndex).Scan(&f.Provider, &at, &ok, &f.Error, &f.Payload)
		if errors.Is(e, sql.ErrNoRows) {
			return nil
		}
		if e != nil {
			return e
		}
		f.At, f.OK, found = time.Unix(0, at).UTC(), ok == 1, true
		return nil
	})
	return f, found, err
}

// PutQuotaFetch records a call to an official quota endpoint.
func (c *Controller) PutQuotaFetch(f QuotaFetch) error {
	return c.withExt(func(db *sql.DB) error {
		ok := 0
		if f.OK {
			ok = 1
		}
		_, err := db.Exec(`INSERT INTO quota_fetch(auth_index, provider, at, ok, error, payload) VALUES(?,?,?,?,?,?)
			ON CONFLICT(auth_index) DO UPDATE SET provider=excluded.provider, at=excluded.at, ok=excluded.ok, error=excluded.error, payload=excluded.payload`,
			f.AuthIndex, f.Provider, f.At.UnixNano(), ok, f.Error, f.Payload)
		return err
	})
}

// ReadMain runs a read-only query against the upstream billing database
// through a separate read-only connection (never writes, never migrates).
func (c *Controller) ReadMain(fn func(*sql.DB) error) error {
	c.mu.Lock()
	path := c.cfg.StateFile
	if c.mainRO == nil || c.mainPath != path {
		if c.mainRO != nil {
			_ = c.mainRO.Close()
		}
		dsn := (&url.URL{Scheme: "file", Path: path, OmitHost: true}).String() + "?mode=ro&_busy_timeout=5000&_query_only=true"
		db, err := sql.Open("sqlite3", dsn)
		if err != nil {
			c.mu.Unlock()
			return fmt.Errorf("open billing database read-only: %w", err)
		}
		db.SetMaxOpenConns(2)
		c.mainRO, c.mainPath = db, path
	}
	db := c.mainRO
	c.mu.Unlock()
	return fn(db)
}

// ClientDaily returns the client module's per-day counters (nil when off).
func (c *Controller) ClientDaily() map[string]ClientDay {
	c.mu.Lock()
	guard := c.cg
	c.mu.Unlock()
	if guard == nil {
		return nil
	}
	out := map[string]ClientDay{}
	for day, d := range guard.DailyCounts() {
		out[day] = ClientDay{Requests: d.Requests, Blocked: d.Blocked}
	}
	return out
}

// ClientDay is one day of client-module counters.
type ClientDay struct {
	Requests int64 `json:"requests"`
	Blocked  int64 `json:"blocked"`
}
