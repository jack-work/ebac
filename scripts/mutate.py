#!/usr/bin/env python3
"""mutate.py: run declared mutants against the tests that claim to catch them (testing standard section 4).

Each mutant is one line of a TSV file (# comments, blank lines ignored):
    id <TAB> file <TAB> line <TAB> old <TAB> new <TAB> package <TAB> test-regex
old must occur EXACTLY ONCE in file, on the declared line; new replaces it ("\\n" and "\\t" are escapes).

Outcomes, one per mutant, four distinct (and the tool's own failure):
    killed           the named tests FAILED with the mutant applied
    not-caught       the named tests PASSED with the mutant applied (the claim is false)
    didnt-apply      old does not occur exactly once in file
    landed-elsewhere old occurs once, but not on the declared line
    didnt-compile    the package does not build with the mutant (no evidence either way)
Exit: 0 every mutant killed; 1 any other outcome; 2 a tool failure (bad input, go missing, tree not restored).
The tree is restored byte for byte after every mutant (checked); one instance runs at a time (flock).
"""
import fcntl, os, subprocess, sys

def esc(s):
    return s.replace("\\n", "\n").replace("\\t", "\t")

def modroot(pkg):
    """The Go module directory holding pkg (a nested go.mod, e.g. ./sdk), and pkg relative to it."""
    parts = pkg.lstrip("./").split("/")
    for i in range(len(parts), 0, -1):
        d = "/".join(parts[:i])
        if os.path.exists(os.path.join(d, "go.mod")):
            rest = "/".join(parts[i:])
            return d, "./" + rest if rest else "."
    return ".", pkg

def main():
    if len(sys.argv) != 2:
        print("usage: mutate.py MUTANTS.tsv", file=sys.stderr); return 2
    root = subprocess.run(["git", "rev-parse", "--show-toplevel"], capture_output=True, text=True)
    if root.returncode:
        print("not in a git tree", file=sys.stderr); return 2
    os.chdir(root.stdout.strip())
    lock = open("/tmp/ebac-mutate.lock", "w")
    try:
        fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
    except OSError:
        print("another mutate.py is running", file=sys.stderr); return 2
    head = subprocess.run(["git", "rev-parse", "HEAD"], capture_output=True, text=True).stdout.strip()
    rows = []
    for n, line in enumerate(open(sys.argv[1]), 1):
        if not line.strip() or line.startswith("#"):
            continue
        f = line.rstrip("\n").split("\t")
        if len(f) != 7:
            print(f"{sys.argv[1]}:{n}: want 7 tab-separated fields, got {len(f)}", file=sys.stderr); return 2
        rows.append(f)
    print(f"# mutants at {head} (working tree: {'clean' if not subprocess.run(['git','status','--porcelain'],capture_output=True,text=True).stdout else 'DIRTY'})")
    worst = 0
    for mid, path, decl, old, new, pkg, rx in rows:
        old, new = esc(old), esc(new)
        orig = open(path, "rb").read()
        text = orig.decode()
        cnt = text.count(old)
        if cnt != 1:
            print(f"{mid}\tdidnt-apply\t{path}: old occurs {cnt} times"); worst = max(worst, 1); continue
        at = text[: text.index(old)].count("\n") + 1
        if str(at) != decl:
            print(f"{mid}\tlanded-elsewhere\t{path}:{at}, declared {decl}"); worst = max(worst, 1); continue
        try:
            open(path, "w").write(text.replace(old, new, 1))
            cwd, gpkg = modroot(pkg)
            b = subprocess.run(["go", "test", "-count=1", "-run", "^$", gpkg], capture_output=True, text=True, cwd=cwd)
            if b.returncode:
                print(f"{mid}\tdidnt-compile\t{path}:{at}: {b.stdout.strip().splitlines()[-1] if b.stdout.strip() else b.stderr.strip()[:200]}")
                worst = max(worst, 1); continue
            t = subprocess.run(["go", "test", "-count=1", "-run", rx, gpkg], capture_output=True, text=True, cwd=cwd)
            fails = [l.split()[2] for l in t.stdout.splitlines() if l.startswith("--- FAIL")]
            if t.returncode and fails:
                print(f"{mid}\tkilled\t{path}:{at}\tby {','.join(sorted(set(fails)))}")
            elif t.returncode:
                print(f"{mid}\tdidnt-compile\t{path}:{at}: tests did not run: {t.stdout.strip()[-200:]}"); worst = max(worst, 1)
            else:
                print(f"{mid}\tnot-caught\t{path}:{at}\t{rx} passed"); worst = max(worst, 1)
        except FileNotFoundError as e:
            print(f"tool failure: {e}", file=sys.stderr); return 2
        finally:
            open(path, "wb").write(orig)
        if open(path, "rb").read() != orig:
            print(f"tool failure: {path} not restored", file=sys.stderr); return 2
    return worst

if __name__ == "__main__":
    sys.exit(main())
