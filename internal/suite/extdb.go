package suite

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// The suite keeps its own data in a separate SQLite file next to the upstream
// database. Upstream's file therefore stays byte-for-byte the upstream schema
// (currently v17) and future upstream migrations (v18+) apply to it without
// ever meeting suite tables. The extension file has its own version counter.
const extSchemaVersion = 4

// extMigrations[i] upgrades the extension database from version i to i+1.
var extMigrations = []string{
	0: `CREATE TABLE suite_meta (key TEXT PRIMARY KEY, value TEXT NOT NULL DEFAULT '')`,
	// v2 (0.5): quota forecast. Official quota readings fetched from the
	// providers' quota endpoints (Codex header readings stay in the upstream
	// request_events table) and the per-account fetch cache / rate limit.
	// Downgrade to v1: DROP both tables and set user_version = 1.
	1: `CREATE TABLE quota_samples (
		auth_index TEXT NOT NULL, provider TEXT NOT NULL, window TEXT NOT NULL,
		window_seconds INTEGER NOT NULL, at INTEGER NOT NULL, used_percent REAL NOT NULL,
		reset_at INTEGER NOT NULL DEFAULT 0, source TEXT NOT NULL DEFAULT '',
		PRIMARY KEY (auth_index, window, at));
	CREATE INDEX quota_samples_at ON quota_samples(at);
	CREATE TABLE quota_fetch (
		auth_index TEXT PRIMARY KEY, provider TEXT NOT NULL, at INTEGER NOT NULL,
		ok INTEGER NOT NULL, error TEXT NOT NULL DEFAULT '', payload TEXT NOT NULL DEFAULT '{}')`,
	// v3 (0.6): passive degradation monitor samples (numbers only, kept
	// 14 days) and the page-only alert center (state, mutes, history 90 days).
	// Downgrade to v2: DROP these six tables (monitor_samples, alerts, alert_mutes, alert_events, forecast_ledger, candy_runs) and set user_version = 2.
	2: `CREATE TABLE monitor_samples (
		id INTEGER PRIMARY KEY AUTOINCREMENT, at INTEGER NOT NULL, auth_index TEXT NOT NULL,
		provider TEXT NOT NULL, model TEXT NOT NULL, response_model TEXT NOT NULL DEFAULT '',
		effort TEXT NOT NULL DEFAULT '', stream INTEGER NOT NULL DEFAULT 0, failed INTEGER NOT NULL DEFAULT 0,
		status INTEGER NOT NULL DEFAULT 0, err_kind TEXT NOT NULL DEFAULT '', output INTEGER NOT NULL DEFAULT 0,
		reasoning INTEGER NOT NULL DEFAULT 0, latency_ms INTEGER NOT NULL DEFAULT 0, ttft_ms INTEGER NOT NULL DEFAULT 0);
	CREATE INDEX monitor_samples_at ON monitor_samples(at);
	CREATE TABLE alerts (
		id TEXT PRIMARY KEY, source TEXT NOT NULL, object TEXT NOT NULL, rule TEXT NOT NULL,
		severity TEXT NOT NULL, status TEXT NOT NULL, payload TEXT NOT NULL DEFAULT '{}',
		first_at INTEGER NOT NULL, last_at INTEGER NOT NULL, resolved_at INTEGER NOT NULL DEFAULT 0,
		count INTEGER NOT NULL DEFAULT 1, ack_at INTEGER NOT NULL DEFAULT 0, ack_by TEXT NOT NULL DEFAULT '',
		ack_severity TEXT NOT NULL DEFAULT '');
	CREATE TABLE alert_mutes (source TEXT NOT NULL, object TEXT NOT NULL, until INTEGER NOT NULL, by TEXT NOT NULL DEFAULT '',
		at INTEGER NOT NULL, PRIMARY KEY (source, object));
	CREATE TABLE alert_events (
		id INTEGER PRIMARY KEY AUTOINCREMENT, alert_id TEXT NOT NULL, at INTEGER NOT NULL, kind TEXT NOT NULL,
		severity TEXT NOT NULL DEFAULT '', by TEXT NOT NULL DEFAULT '', payload TEXT NOT NULL DEFAULT '{}');
	CREATE INDEX alert_events_at ON alert_events(at);
	CREATE TABLE forecast_ledger (
		auth_index TEXT NOT NULL, provider TEXT NOT NULL, window TEXT NOT NULL, start INTEGER NOT NULL, end INTEGER NOT NULL,
		early INTEGER NOT NULL DEFAULT 0, peak REAL NOT NULL, usd REAL NOT NULL, tokens INTEGER NOT NULL, requests INTEGER NOT NULL,
		capacity_usd REAL, capacity_tokens REAL, samples INTEGER NOT NULL DEFAULT 0, confidence TEXT NOT NULL DEFAULT '',
		PRIMARY KEY (auth_index, window, end));
	CREATE TABLE candy_runs (
		id INTEGER PRIMARY KEY AUTOINCREMENT, auth_index TEXT NOT NULL, provider TEXT NOT NULL, model TEXT NOT NULL,
		effort TEXT NOT NULL DEFAULT '', started_at INTEGER NOT NULL, finished_at INTEGER NOT NULL DEFAULT 0,
		status TEXT NOT NULL, passed INTEGER NOT NULL DEFAULT 0, answer TEXT NOT NULL DEFAULT '', error TEXT NOT NULL DEFAULT '',
		input_tokens INTEGER NOT NULL DEFAULT 0, output_tokens INTEGER NOT NULL DEFAULT 0, reasoning_tokens INTEGER NOT NULL DEFAULT 0,
		latency_ms INTEGER NOT NULL DEFAULT 0, response_model TEXT NOT NULL DEFAULT '');
	CREATE INDEX candy_runs_auth ON candy_runs(auth_index, started_at)`,
	// v4 (0.7): input tokens per monitor sample (degradation strata) and
	// alert debounce state (hits = consecutive detections while pending,
	// missing_since = first evaluation that no longer saw an active alert).
	// Status gains "pending" (detected, not yet opened).
	// Downgrade to v3: scripts/suite_ext_downgrade_v3.py (rebuilds both tables
	// without the new columns, drops pending alerts, sets user_version = 3).
	3: `ALTER TABLE monitor_samples ADD COLUMN input INTEGER NOT NULL DEFAULT 0;
	ALTER TABLE alerts ADD COLUMN hits INTEGER NOT NULL DEFAULT 0;
	ALTER TABLE alerts ADD COLUMN missing_since INTEGER NOT NULL DEFAULT 0`,
}

// ExtPath derives the extension database path from the upstream state file.
func ExtPath(stateFile string) string {
	base := strings.TrimSuffix(stateFile, filepath.Ext(stateFile))
	return base + ".suite-ext.db"
}

type extDB struct{ db *sql.DB }

func openExt(path string) (*extDB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	dsn := (&url.URL{Scheme: "file", Path: path, OmitHost: true}).String() + "?_busy_timeout=5000&_journal_mode=WAL&_txlock=immediate"
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	tx, err := db.Begin()
	if err != nil {
		db.Close()
		return nil, err
	}
	var version int
	if err := tx.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		tx.Rollback()
		db.Close()
		return nil, err
	}
	if version > extSchemaVersion {
		tx.Rollback()
		db.Close()
		return nil, fmt.Errorf("Suite extension database %s is v%d, newer than this build (v%d)", path, version, extSchemaVersion)
	}
	for ; version < extSchemaVersion; version++ {
		if _, err := tx.Exec(extMigrations[version]); err != nil {
			tx.Rollback()
			db.Close()
			return nil, fmt.Errorf("Migrate suite extension database to v%d: %w", version+1, err)
		}
	}
	if _, err := tx.Exec(fmt.Sprintf("PRAGMA user_version = %d", extSchemaVersion)); err != nil {
		tx.Rollback()
		db.Close()
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		db.Close()
		return nil, err
	}
	_ = os.Chmod(path, 0o600)
	return &extDB{db: db}, nil
}

func (e *extDB) close() error { return e.db.Close() }

func (e *extDB) put(key string, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	_, err = e.db.Exec(`INSERT INTO suite_meta(key, value) VALUES(?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, string(raw))
	return err
}

func (e *extDB) get(key string, target any) (bool, error) {
	var raw string
	err := e.db.QueryRow(`SELECT value FROM suite_meta WHERE key = ?`, key).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, json.Unmarshal([]byte(raw), target)
}
