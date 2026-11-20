#!/usr/bin/env python3
"""Tests for ghcache-shadow-diff.py: python3 scripts/test_ghcache_shadow_diff.py (exit 0 = pass)."""
import copy
import importlib.util
import os
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
spec = importlib.util.spec_from_file_location("sd", os.path.join(HERE, "ghcache-shadow-diff.py"))
sd = importlib.util.module_from_spec(spec)
spec.loader.exec_module(sd)

BOARD = {"author": "john-kelliher", "base": "main", "branch": "b", "draft": False, "head": "aa", "merge_state": "BLOCKED",
         "mergeable": "MERGEABLE", "state": "OPEN", "title": "t" * 130, "updated_at": "2026-11-20T00:00:00Z",
         "created_at": "2026-11-01T00:00:00Z", "tree": "tt", "lines": [1, 2], "ready_at": None, "fixes": [3, 1],
         "arch": [{"verdict": "approve", "at": "x", "at_head": True, "sha": "aa"}], "praxis": None,
         "checks": {"Aether Rust PR Pipeline": {"state": "pass", "run_id": 1, "jobs_failed": []}, "ADO only": {"state": "pass"}}}
CACHE = {"gh": {"author": "john-kelliher", "base": "main", "branch": "b", "draft": False, "head": "aa", "merge_state": "BLOCKED",
                "mergeable": "MERGEABLE", "state": "OPEN", "title": "t" * 200, "updated_at": "2026-11-20T00:00:00Z",
                "created_at": "2026-11-01T00:00:00Z", "tree": "tt", "lines": [1, 2], "fixes": [1, 3],
                "checks": {"Aether Rust PR Pipeline": {"state": "pass"}, "banana/premerge": {"state": "pass"}}},
         "parsed": {"bots": {"architect": [{"verdict": "approve", "at": "x", "at_head": True, "sha": "aa"}]}}}

fails = []


def check(name, got, want):
    if got != want:
        fails.append(f"{name}: got {got!r}, want {want!r}")


# Agreeing rows: no disagreement; title cut at 120 on the cache side, fixes order ignored; one-sided pipelines are gap.
d, gap = sd.compare(BOARD, CACHE)
check("agree diffs", d, [])
check("agree gap", gap, [("ADO only", "board only"), ("banana/premerge", "cache only")])

# A head disagreement is open inside the grace, stale past it, and forgotten once it agrees.
c2 = copy.deepcopy(CACHE)
c2["gh"]["head"] = "bb"
seen = {}
r = sd.snapshot({"1": BOARD}, {"1": c2}, seen, 1000, 900)
check("open", [(x["field"], x["board"], x["cache"]) for x in r["open"]], [("head", "aa", "bb")])
check("not stale yet", r["stale"], [])
r = sd.snapshot({"1": BOARD}, {"1": c2}, seen, 1900, 900)
check("at the grace still open", len(r["open"]), 1)
r = sd.snapshot({"1": BOARD}, {"1": c2}, seen, 1901, 900)
check("past the grace stale", [(x["field"], x["since"]) for x in r["stale"]], [("head", 1000)])
r = sd.snapshot({"1": BOARD}, {"1": CACHE}, seen, 2000, 900)
check("agreed again", (r["open"], r["stale"], seen), ([], [], {}))

# A shared pipeline in a different state is a disagreement; an architect verdict missing on one side is one.
c3 = copy.deepcopy(CACHE)
c3["gh"]["checks"]["Aether Rust PR Pipeline"]["state"] = "fail"
c3["parsed"]["bots"] = {}
d, _ = sd.compare(BOARD, c3)
check("checks and arch", [x[0] for x in d], ["arch", "checks.Aether Rust PR Pipeline"])

# PRs on one side only are listed, never compared.
r = sd.snapshot({"1": BOARD, "2": BOARD}, {"1": CACHE, "3": CACHE}, {}, 0, 900)
check("sets", (r["both"], r["only_board"], r["only_cache"]), (1, ["2"], ["3"]))

print("\n".join(fails) or "ok: 11 checks")
sys.exit(1 if fails else 0)
