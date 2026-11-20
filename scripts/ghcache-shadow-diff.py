#!/usr/bin/env python3
"""ghcache-shadow-diff: compare the GitHub cache's entity forms with the board's pr.<n> rows (ghcache slice 1 proof).

usage: ghcache-shadow-diff.py [--board @zombie-phrasing] [--prefix ebac-gh-cache-] [--state FILE] [--out FILE]
                              [--grace SECONDS]

One snapshot per run (a timer runs it every 5 min for 24 h). For every PR on both sides it compares the fields the
board and the cache share; a disagreement is STALE when the same (pr, field) has disagreed for longer than --grace
(default 900 s: one board snapshot interval plus the cache's hot tier). Appends one JSON line per run to --out:
{at, board_prs, cache_prs, both, only_board[], only_cache[], open[], stale[], checks_gap[]}; prints a one-line summary.
Exit 0 with no stale disagreement, 1 with some, 2 when a form cannot be read (tool failure, never "clean").

checks are compared by pipeline state only where both sides name the pipeline; the board reads ADO, the cache GitHub's
check runs, so a pipeline on one side only is a checks_gap line, not a disagreement.
"""
import argparse
import json
import os
import subprocess
import sys
import time

FIELDS = ["author", "base", "branch", "draft", "head", "merge_state", "mergeable", "state", "title", "updated_at",
          "created_at", "tree", "lines", "ready_at", "fixes"]


def form(fid):
    r = subprocess.run(["figaro", "state", "show", "--id", fid, "-j"], capture_output=True, text=True, timeout=60)
    if r.returncode != 0:
        raise RuntimeError(f"figaro state show {fid}: status={r.returncode} {r.stderr.strip()[:200]}")
    return json.loads(r.stdout)


def cache_forms(prefix):
    r = subprocess.run(["figaro", "form", "ls", "-j"], capture_output=True, text=True, timeout=60)
    if r.returncode != 0:
        raise RuntimeError(f"figaro form ls: status={r.returncode} {r.stderr.strip()[:200]}")
    out = {}
    for f in json.loads(r.stdout):
        name = f.get("name") or ""
        if name.startswith(prefix) and "-pr-" in name:
            out[name] = f["id"]
    return out


def ours(entity):
    """The board-shaped view of one entity form."""
    gh, parsed = entity.get("gh") or {}, entity.get("parsed") or {}
    row = {k: gh.get(k) for k in FIELDS}
    row["title"] = (gh.get("title") or "")[:120]
    row["ready_at"] = gh.get("ready_at") or None
    row["fixes"] = sorted(gh.get("fixes") or [])
    bots = parsed.get("bots") or {}
    row["arch"] = bots.get("architect") or []
    row["praxis"] = bots.get("praxis")
    row["checks"] = {k: v.get("state") for k, v in (gh.get("checks") or {}).items()}
    return row


def theirs(r):
    row = {k: r.get(k) for k in FIELDS}
    row["fixes"] = sorted(r.get("fixes") or [])
    row["arch"] = [{k: a.get(k) for k in ("verdict", "at", "at_head", "sha")} for a in r.get("arch") or []]
    px = r.get("praxis")
    row["praxis"] = px
    row["checks"] = {k: v.get("state") for k, v in (r.get("checks") or {}).items()}
    return row


def compare(board_row, cache_row):
    """[(field, board value, cache value)] of every disagreement, and [(pipeline, side)] of the checks gap."""
    b, c = theirs(board_row), ours(cache_row)
    diffs = [(f, b[f], c[f]) for f in FIELDS + ["arch", "praxis"] if b[f] != c[f]]
    gap = []
    for name in sorted(set(b["checks"]) | set(c["checks"])):
        if name not in c["checks"]:
            gap.append((name, "board only"))
        elif name not in b["checks"]:
            gap.append((name, "cache only"))
        elif b["checks"][name] != c["checks"][name]:
            diffs.append(("checks." + name, b["checks"][name], c["checks"][name]))
    return diffs, gap


def snapshot(board, cache, seen, now, grace):
    """One run over loaded rows: board {n: row}, cache {n: entity}. seen {pr:field: first_at} is updated in place."""
    both = sorted(set(board) & set(cache), key=int)
    rec = {"at": now, "board_prs": len(board), "cache_prs": len(cache), "both": len(both),
           "only_board": sorted(set(board) - set(cache), key=int), "only_cache": sorted(set(cache) - set(board), key=int),
           "open": [], "stale": [], "checks_gap": []}
    live = set()
    for n in both:
        diffs, gap = compare(board[n], cache[n])
        rec["checks_gap"] += [f"{n}:{p} ({side})" for p, side in gap]
        for f, bv, cv in diffs:
            k = f"{n}:{f}"
            live.add(k)
            first = seen.setdefault(k, now)
            line = {"pr": int(n), "field": f, "board": bv, "cache": cv, "since": first}
            rec["stale" if now - first > grace else "open"].append(line)
    for k in list(seen):
        if k not in live:
            del seen[k]
    return rec


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--board", default="@zombie-phrasing")
    ap.add_argument("--prefix", default="ebac-gh-cache-")
    ap.add_argument("--state", default=os.path.expanduser("~/.local/state/ghcache-shadow/seen.json"))
    ap.add_argument("--out", default=os.path.expanduser("~/.local/state/ghcache-shadow/diff.jsonl"))
    ap.add_argument("--grace", type=int, default=900)
    a = ap.parse_args()
    try:
        board = {str(n): r for n, r in (form(a.board).get("pr") or {}).items()}
        cache = {}
        for name, fid in cache_forms(a.prefix).items():
            e = form(fid)
            n = (e.get("gh") or {}).get("number")
            if n:
                cache[str(n)] = e
    except (RuntimeError, json.JSONDecodeError, subprocess.TimeoutExpired) as err:
        print(f"ghcache-shadow-diff: tool failure: {err}", file=sys.stderr)
        return 2
    if not board or not cache:
        print(f"ghcache-shadow-diff: tool failure: {len(board)} board rows, {len(cache)} cache forms", file=sys.stderr)
        return 2
    os.makedirs(os.path.dirname(a.state), exist_ok=True)
    seen = json.load(open(a.state)) if os.path.exists(a.state) else {}
    rec = snapshot(board, cache, seen, int(time.time()), a.grace)
    json.dump(seen, open(a.state, "w"))
    with open(a.out, "a") as f:
        f.write(json.dumps(rec, sort_keys=True) + "\n")
    print(f"board {rec['board_prs']} cache {rec['cache_prs']} both {rec['both']}: {len(rec['stale'])} stale, "
          f"{len(rec['open'])} open, {len(rec['checks_gap'])} checks gap; {a.out}")
    return 1 if rec["stale"] else 0


if __name__ == "__main__":
    sys.exit(main())
