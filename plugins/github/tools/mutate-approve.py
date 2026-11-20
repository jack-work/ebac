#!/usr/bin/env python3
"""Mutants of #27 design B (two-phase approve): each must turn a named test red.

Run: python3 plugins/github/tools/mutate-approve.py <ebac repo root>  (go + the module cache in the environment; a
build node, never gluk). First the baseline: every package below must be green unmutated, else exit 2 (a red baseline
"kills" everything). Then one line per mutant: KILLED <failing tests> | SURVIVED | ERROR(no anchor) | ERROR(build).
Exit 1 if any mutant is not KILLED, 2 on a tool failure."""
import os
import re
import shutil
import subprocess
import sys
import tempfile

A = "plugins/github/internal/approve/"
O = "plugins/github/internal/orgapprove/"
P = "internal/poll/"
# (name, file, old, new); old must occur exactly once.
MUTANTS = [
    ("T1 approve commit ignores the token", A + "approve.go",
     "if rec.Token == p.Token && rec.PostedAt == \"\" {", "if rec.PostedAt == \"\" {"),
    ("T2 org commit ignores the token", O + "orgapprove.go",
     "a.Post.Token == p.Token && a.Post.PostedAt == \"\"", "a.Post.PostedAt == \"\""),
    ("S1 core commits after a failed save", P + "commit.go", "\tif !saved {\n", "\tif !saved && false {\n"),
    ("S2 core allows Commit for any source", P + "commit.go", "\tcase !CommitSources[src]:\n", "\tcase false && !CommitSources[src]:\n"),
    ("S3 a commit reply may ask Commit again", P + "commit.go", "\tcase op == plugin.OpCommit:\n", "\tcase false && op == plugin.OpCommit:\n"),
    ("R1 approve drops the record", A + "approve.go", "\tout.Posts[id] = &model.PostRecord{", "\t_ = &model.PostRecord{"),
    ("R2 org drops the record", O + "orgapprove.go", "\t\t\ta.Post = &model.PostRecord{", "\t\t\t_ = &model.PostRecord{"),
    ("M1 approve marker only on APPROVED", A + "approve.go",
     "\t\tif strings.Contains(rs[i].Body, marker) {\n", "\t\tif rs[i].State == \"APPROVED\" && strings.Contains(rs[i].Body, marker) {\n"),
    ("M2 org marker only on APPROVED", O + "orgapprove.go",
     "\t\tif strings.Contains(r.Body, approve.Marker(id)) ||", "\t\tif (r.State == \"APPROVED\" && strings.Contains(r.Body, approve.Marker(id))) ||"),
    ("A1 approve drops the ask-body match", A + "approve.go",
     "\tif rv := forAsk(reviews, Body(w.Asker(), r.MsgID)); rv != nil {\n", "\tif rv := forAsk(reviews, Body(w.Asker(), r.MsgID)); rv != nil && false {\n"),
    ("G1 single review page", A + "gh.go", "\t\tif len(rs) < ReviewsPerPage {\n", "\t\tif true {\n"),
    ("C1 approve checks re-run after the record", A + "approve.go",
     "\t\tif rec := out.Posts[id]; rec != nil {\n", "\t\tif rec := out.Posts[id]; rec != nil && false {\n"),
    ("C2 org checks re-run after the record", O + "orgapprove.go", "\t\tif a.Post != nil {\n", "\t\tif a.Post != nil && false {\n"),
    ("L1 approve never gives up a lost post", A + "approve.go",
     "\tif perr != nil || p.Now.Sub(t) >= LostAfter {\n", "\tif (perr != nil || p.Now.Sub(t) >= LostAfter) && false {\n"),
    ("L2 org never gives up a lost post", O + "orgapprove.go",
     "\tif perr != nil || p.Now.Sub(t) >= LostAfter {\n", "\tif (perr != nil || p.Now.Sub(t) >= LostAfter) && false {\n"),
]

# (module dir, packages) each mutant is judged by.
SUITES = [("plugins/github", ["./internal/approve/", "./internal/orgapprove/"]), (".", ["./internal/poll/"])]


def test(tree):
    """(ok, failing test names, output) over every suite."""
    fails, out, ok = [], "", True
    for mod, pkgs in SUITES:
        r = subprocess.run(["go", "test", "-count=1"] + pkgs, cwd=os.path.join(tree, mod), capture_output=True, text=True)
        out += r.stdout + r.stderr
        if r.returncode != 0:
            ok = False
            fails += re.findall(r"^--- FAIL: (\S+)", r.stdout, re.M)
    return ok, fails, out


def main():
    root = os.path.abspath(sys.argv[1] if len(sys.argv) > 1 else ".")
    if len(MUTANTS) < 15:
        print(f"only {len(MUTANTS)} mutants", file=sys.stderr)
        sys.exit(2)
    ok, fails, out = test(root)
    if not ok:
        print("baseline red: " + ",".join(fails[:5]) + "\n" + out[-2000:])
        sys.exit(2)
    print("baseline green", flush=True)
    bad = 0
    for name, path, old, new in MUTANTS:
        with tempfile.TemporaryDirectory() as d:
            tree = os.path.join(d, "ebac")
            shutil.copytree(root, tree, symlinks=True, ignore=shutil.ignore_patterns(".git"))
            f = os.path.join(tree, path)
            os.chmod(f, 0o644)
            text = open(f).read()
            if text.count(old) != 1:
                print(f"ERROR(no anchor) {name} ({text.count(old)} matches)", flush=True)
                bad += 1
                continue
            open(f, "w").write(text.replace(old, new))
            ok, fails, out = test(tree)
            if not ok and not fails:
                last = [l for l in out.strip().splitlines() if l.strip()][-1:] or ["?"]
                print(f"ERROR(build) {name}: {last[0][:200]}", flush=True)
                bad += 1
            elif ok:
                print(f"SURVIVED {name}", flush=True)
                bad += 1
            else:
                print(f"KILLED {name}: {','.join(sorted(set(fails))[:4])}", flush=True)
    print(f"status={1 if bad else 0}")
    sys.exit(1 if bad else 0)


if __name__ == "__main__":
    main()
