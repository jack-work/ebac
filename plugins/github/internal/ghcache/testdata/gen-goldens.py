#!/usr/bin/env python3
"""Generate golden-ref.json: the Lead's Python parsers (the reference this package ports) run over deep-6.json.

Run from this directory: python3 gen-goldens.py ~/dev/decisions-tools > golden-ref.json
architect = board-snapshot.arch_verdicts, praxis = board-snapshot.praxis_of (approvers as filters.toml:
architect-review, ilya-grebnov); lair = pr-body-evidence.peer_line's per-reviewer pick with its ruling(),
the sha unsliced. Regenerate only when the reference changes; the Go test compares field by field.
"""
import importlib.util
import json
import os
import re
import sys

tools = sys.argv[1]
sys.path.insert(0, tools)


def load(name, file):
    spec = importlib.util.spec_from_file_location(name, os.path.join(tools, file))
    m = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(m)
    return m


bs = load("bs", "board-snapshot.py")
pbe = load("pbe", "pr-body-evidence.py")
F = {"approvers": {"logins": ["architect-review", "ilya-grebnov"]}}
data = json.load(open("deep-6.json"))["data"]["repository"]
out = {}
for alias, v in sorted(data.items()):
    head = v["headRefOid"]
    c = v["commits"]["nodes"][0]["commit"] if v["commits"]["nodes"] else {}
    tree = (c.get("tree") or {}).get("oid")
    lair = {}
    for cm in v["comments"]["nodes"]:
        m = re.match(r"^## (yugo|ken|mike)\b.*?(?:head|at) ([0-9a-f]{7,40})", cm.get("body") or "", re.I | re.S)
        if m:
            lair[m.group(1).lower()] = {"ruling": pbe.ruling(cm.get("body") or ""), "sha": m.group(2), "url": cm.get("url", "")}
    out[str(v["number"])] = {
        "architect": bs.arch_verdicts(v, F, head, tree),
        "praxis": bs.praxis_of(v, head),
        "lair": lair,
    }
json.dump(out, sys.stdout, indent=1, sort_keys=True)
print()
