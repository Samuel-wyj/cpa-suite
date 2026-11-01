#!/usr/bin/env python3
"""Downgrade the CPA Suite extension DB from v4 (0.7) to v3 (0.6) before
rolling back the .so. 0.6 refuses an ext DB newer than v3, so run this with
CPA STOPPED. Keeps all data: monitor_samples loses only the new `input` column,
alerts loses the debounce columns (`hits`, `missing_since`) and alerts still
pending (detected once, never opened) and info-level notes are dropped. Saved alert thresholds stay
in suite_meta (0.6 ignores them). Idempotent; a v3 DB is left untouched.
Works with old SQLite (CentOS 7 python3 ships 3.7.17): tables are rebuilt,
not ALTER ... DROP COLUMN. To go further back to 0.5, run
suite_ext_downgrade_v2.py afterwards.
  python3 suite_ext_downgrade_v3.py /path/to/cpa/plugins/cpa-suite.suite-ext.db"""
import sqlite3, sys
if len(sys.argv) != 2:
    sys.exit(__doc__)
db = sqlite3.connect(sys.argv[1], timeout=5, isolation_level=None)
v = db.execute("PRAGMA user_version").fetchone()[0]
if v == 3:
    print("already v3, nothing to do"); sys.exit(0)
if v != 4:
    sys.exit(f"unexpected ext schema version {v}; refusing (restore the backup instead)")
db.execute("BEGIN IMMEDIATE")
try:
    db.execute("""CREATE TABLE monitor_samples_v3 (
		id INTEGER PRIMARY KEY AUTOINCREMENT, at INTEGER NOT NULL, auth_index TEXT NOT NULL,
		provider TEXT NOT NULL, model TEXT NOT NULL, response_model TEXT NOT NULL DEFAULT '',
		effort TEXT NOT NULL DEFAULT '', stream INTEGER NOT NULL DEFAULT 0, failed INTEGER NOT NULL DEFAULT 0,
		status INTEGER NOT NULL DEFAULT 0, err_kind TEXT NOT NULL DEFAULT '', output INTEGER NOT NULL DEFAULT 0,
		reasoning INTEGER NOT NULL DEFAULT 0, latency_ms INTEGER NOT NULL DEFAULT 0, ttft_ms INTEGER NOT NULL DEFAULT 0)""")
    cols = "id, at, auth_index, provider, model, response_model, effort, stream, failed, status, err_kind, output, reasoning, latency_ms, ttft_ms"
    db.execute(f"INSERT INTO monitor_samples_v3({cols}) SELECT {cols} FROM monitor_samples")
    db.execute("DROP TABLE monitor_samples")
    db.execute("ALTER TABLE monitor_samples_v3 RENAME TO monitor_samples")
    db.execute("CREATE INDEX monitor_samples_at ON monitor_samples(at)")
    db.execute("""CREATE TABLE alerts_v3 (
		id TEXT PRIMARY KEY, source TEXT NOT NULL, object TEXT NOT NULL, rule TEXT NOT NULL,
		severity TEXT NOT NULL, status TEXT NOT NULL, payload TEXT NOT NULL DEFAULT '{}',
		first_at INTEGER NOT NULL, last_at INTEGER NOT NULL, resolved_at INTEGER NOT NULL DEFAULT 0,
		count INTEGER NOT NULL DEFAULT 1, ack_at INTEGER NOT NULL DEFAULT 0, ack_by TEXT NOT NULL DEFAULT '',
		ack_severity TEXT NOT NULL DEFAULT '')""")
    cols = "id, source, object, rule, severity, status, payload, first_at, last_at, resolved_at, count, ack_at, ack_by, ack_severity"
    # knows only medium/high: info-level notes (0.7) are dropped too.
    db.execute(f"INSERT INTO alerts_v3({cols}) SELECT {cols} FROM alerts WHERE status != 'pending' AND severity != 'info'")
    db.execute("DROP TABLE alerts")
    db.execute("ALTER TABLE alerts_v3 RENAME TO alerts")
    db.execute("PRAGMA user_version = 3")
    db.execute("COMMIT")
except Exception:
    db.execute("ROLLBACK")
    raise
db.execute("PRAGMA wal_checkpoint(TRUNCATE)")
n = db.execute("SELECT COUNT(*) FROM monitor_samples").fetchone()[0]
a = db.execute("SELECT COUNT(*) FROM alerts").fetchone()[0]
print(f"ext DB downgraded to v3; monitor_samples {n} rows, alerts {a} rows kept")
