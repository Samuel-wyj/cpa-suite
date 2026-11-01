#!/usr/bin/env python3
"""Normalise a parity transcript so upstream and suite runs can be diffed.

Only differences that are expected by design are removed:
  * the plugin ID (cpa-key-billing vs cpa-suite) in messages and paths,
  * per-run temporary directories,
  * randomly generated quota window IDs (32 hex chars, crypto/rand).
Everything else -- status codes, costs, token splits, cycles, picks -- must match.
"""
import base64, json, re, sys

hex32 = re.compile(r'\b[0-9a-f]{32}\b')
ids = {}

def norm_str(s, root):
    s = s.replace(root, '<DIR>')
    s = s.replace('cpa-key-billing', '<PLUGIN>').replace('cpa-suite', '<PLUGIN>')
    return hex32.sub(lambda m: ids.setdefault(m.group(0), f'<ID{len(ids)+1}>'), s)

def walk(v, root):
    if isinstance(v, dict):
        out = {}
        for k, x in v.items():
            if k == 'ResponseBody' and isinstance(x, str):
                try:
                    x = json.loads(base64.b64decode(x))
                except Exception:
                    pass
            out[norm_str(k, root)] = walk(x, root)
        return out
    if isinstance(v, list):
        return [walk(x, root) for x in v]
    if isinstance(v, str):
        try:
            inner = json.loads(v)
            if isinstance(inner, (dict, list)):
                return walk(inner, root)
        except Exception:
            pass
        return norm_str(v, root)
    return v

path, root = sys.argv[1], sys.argv[2]
data = json.load(open(path))
json.dump(walk(data, root), sys.stdout, indent=1, ensure_ascii=False, sort_keys=True)
print()
