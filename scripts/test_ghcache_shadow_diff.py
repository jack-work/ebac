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
         "parsed": {"bots": {"architect": [{"verdict": "approve", "at": "x", "at_head": True, "sha": "aa", "via": "comment", "by": "architect-review"},
                                           # Ilya's own review: the board never reads it, so it is no disagreement
                                           {"verdict": "approve", "at": "y", "at_head": True, "sha": "aa", "via": "review", "by": "ilya-grebnov"}]}}}

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

# A missing sha is None on both sides (Go omits it, the board writes null).
b4, c4 = copy.deepcopy(BOARD), copy.deepcopy(CACHE)
b4["arch"][0]["sha"] = None
del c4["parsed"]["bots"]["architect"][0]["sha"]
check("sha None vs omitted", sd.compare(b4, c4)[0], [])

# The board's architect window is narrower: a cache verdict older than the board's oldest is no disagreement
# (the shadow's 251/254 arch rows); a cache missing the board's newest is one; so is a differing verdict inside
# the window. RED if the window cut goes (first row) or cuts inside the window (the last two rows).
OLD = {"verdict": "approve", "at": "a", "at_head": False, "sha": "11", "via": "comment", "by": "architect-review"}
c5 = copy.deepcopy(CACHE)
c5["parsed"]["bots"]["architect"].insert(0, OLD)
check("arch: older cache verdict outside the board window", sd.compare(BOARD, c5)[0], [])
b6 = copy.deepcopy(BOARD)
b6["arch"].append({"verdict": "request_changes", "at": "z", "at_head": True, "sha": "bb"})
check("arch: cache misses the board's newest", [x[0] for x in sd.compare(b6, c5)[0]], ["arch"])
c7 = copy.deepcopy(c5)
c7["parsed"]["bots"]["architect"][1]["verdict"] = "request_changes"
check("arch: a differing verdict inside the window", [x[0] for x in sd.compare(BOARD, c7)[0]], ["arch"])

# A board PR the cache lacks is a "tracked" disagreement (stale past the grace: the proof fails); the cache's extra
# PRs are only listed.
seen = {}
r = sd.snapshot({"1": BOARD, "2": BOARD}, {"1": CACHE, "3": CACHE}, seen, 0, 900)
check("sets", (r["both"], r["only_board"], r["only_cache"]), (1, ["2"], ["3"]))
check("only_board open", [(x["pr"], x["field"]) for x in r["open"]], [(2, "tracked")])
r = sd.snapshot({"1": BOARD, "2": BOARD}, {"1": CACHE, "3": CACHE}, seen, 901, 900)
check("only_board stale", [(x["pr"], x["field"]) for x in r["stale"]], [(2, "tracked")])

# Forms of another repo are never read: the name filter is <prefix><repo key>-pr-.
check("repo key", sd.repo_key("bic/aether"), "aether")
check("repo key odd", sd.repo_key("bic/My.Repo"), "my-repo")

print("\n".join(fails) or "ok: 19 checks")
sys.exit(1 if fails else 0)
