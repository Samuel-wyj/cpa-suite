#!/usr/bin/env python3
"""Downgrade the CPA Suite extension DB from v3 (0.6) to v2 (0.5) before
rolling back the .so. 0.5 refuses an ext DB newer than v2, so run this with
CPA STOPPED. Drops only the 0.6 tables (monitor_samples, alerts, alert_mutes, alert_events,
forecast_ledger, candy_runs: monitor metadata, alert history, the cycle ledger and candy-test results are lost); forecast samples and
client-guard data are kept. Idempotent; a v2 DB is left untouched. To go further
back to 0.4, run suite_ext_downgrade_v1.py afterwards.
  python3 suite_ext_downgrade_v2.py /path/to/cpa/plugins/cpa-suite.suite-ext.db"""
import sqlite3, sys
if len(sys.argv) != 2:
    sys.exit(__doc__)
db = sqlite3.connect(sys.argv[1], timeout=5)
v = db.execute("PRAGMA user_version").fetchone()[0]
if v == 2:
    print("already v2, nothing to do"); sys.exit(0)
if v != 3:
    sys.exit(f"unexpected ext schema version {v}; refusing (restore the backup instead)")
db.execute("BEGIN IMMEDIATE")
for t in ("monitor_samples", "alerts", "alert_mutes", "alert_events", "forecast_ledger", "candy_runs"):
    db.execute(f"DROP TABLE IF EXISTS {t}")
db.execute("PRAGMA user_version = 2")
db.commit()
db.execute("PRAGMA wal_checkpoint(TRUNCATE)")
tables = [r[0] for r in db.execute("SELECT name FROM sqlite_master WHERE type='table' ORDER BY name")]
print("ext DB downgraded to v2; tables:", ", ".join(tables))
