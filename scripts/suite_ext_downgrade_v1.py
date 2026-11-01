#!/usr/bin/env python3
"""Downgrade the CPA Suite extension DB from v2 (0.5) to v1 (0.4) before
rolling back the .so. 0.4 refuses an ext DB newer than v1, so run this with
CPA STOPPED. Drops only the 0.5 forecast tables (quota_samples, quota_fetch:
cached official quota readings, safe to lose); client-guard rules and stats live
in suite_meta and are kept. Idempotent; a v1 DB is left untouched.
  python3 suite_ext_downgrade_v1.py /path/to/cpa/plugins/cpa-suite.suite-ext.db"""
import sqlite3, sys
if len(sys.argv) != 2:
    sys.exit(__doc__)
db = sqlite3.connect(sys.argv[1], timeout=5)
v = db.execute("PRAGMA user_version").fetchone()[0]
if v == 1:
    print("already v1, nothing to do"); sys.exit(0)
if v != 2:
    sys.exit(f"unexpected ext schema version {v}; refusing (restore the backup instead)")
db.execute("BEGIN IMMEDIATE")
db.execute("DROP TABLE IF EXISTS quota_samples")
db.execute("DROP TABLE IF EXISTS quota_fetch")
db.execute("PRAGMA user_version = 1")
db.commit()
db.execute("PRAGMA wal_checkpoint(TRUNCATE)")
tables = [r[0] for r in db.execute("SELECT name FROM sqlite_master WHERE type='table' ORDER BY name")]
print("ext DB downgraded to v1; tables:", ", ".join(tables))
