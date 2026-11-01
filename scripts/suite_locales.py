#!/usr/bin/env python3
"""Merges CPA Suite UI strings (internal/suite/locales/*.json) into the upstream
catalogs internal/plugin/web/locales/*.json. Idempotent: re-run after an
upstream sync; suite.* keys are appended at the end, upstream keys untouched."""
import json, os, sys
root = os.path.join(os.path.dirname(os.path.abspath(__file__)), "..")
check = "--check" in sys.argv
bad = False
for lang in ("en", "zh-CN"):
    up_path = os.path.join(root, "internal/plugin/web/locales", lang + ".json")
    up = json.load(open(up_path, encoding="utf-8"))
    suite = json.load(open(os.path.join(root, "internal/suite/locales", lang + ".json"), encoding="utf-8"))
    merged = {k: v for k, v in up.items() if not k.startswith("suite.")}
    merged.update(suite)
    text = json.dumps(merged, ensure_ascii=False, indent=2) + "\n"
    if check:
        if open(up_path, encoding="utf-8").read() != text:
            print("out of date:", up_path); bad = True
    else:
        open(up_path, "w", encoding="utf-8").write(text)
sys.exit(1 if bad else 0)
