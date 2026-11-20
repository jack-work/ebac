#!/usr/bin/env bash
# ghcache-mutants.sh: run the ghcache slice 1 mutants (testing standard 4.3). For a build node, never gluk.
# Each mutant: a file, a literal old line fragment, its replacement, the package to test.
# Outcomes, one line each: KILLED <test> | NOT-CAUGHT | DIDNT-APPLY | DIDNT-COMPILE. Restores each file byte for byte
# from a copy (cmp checked after each mutant; no git needed: it runs in a nix build); exits 2 on a tool failure or an
# unrestored file, 1 when any mutant is not killed. Runs from the repo root (the directory above this script).
set -u
cd "$(dirname "$0")/.." || exit 2
LOCK=${TMPDIR:-/tmp}/ghcache-mutants.lock
exec 9>"$LOCK"; flock -n 9 || { echo "another run holds the lock" >&2; exit 2; }
BK=$(mktemp -d) || exit 2
G=plugins/github/internal/ghcache
MUTANTS=(
  "C1|internal/poll/forms.go|return src == model.SourceKeelBranches \|\| src == model.SourceGHCache|return src == model.SourceKeelBranches|./internal/poll/"
  "C2|internal/poll/poll.go|model.SourceGHCache: {\"track\"}|model.SourceGHCache: {\"track\", \"config\"}|./internal/poll/"
  "C3|sdk/plugin/protocol.go|r.GHCache = f.GHCache|_ = f.GHCache|./internal/pluginrun/"
  "P1|$G/parse.go|archMark     = \"<!-- architect-review -->\"|archMark     = \"<!-- architect -->\"|./$G/"
  "P2|$G/parse.go|v.Sha, v.AtHead = s[1], atHead(s[1], head)|v.Sha, v.AtHead = s[1], true|./$G/"
  "P3|$G/parse.go|(b.Praxis == nil \|\| c.CreatedAt > b.Praxis.At)|(b.Praxis == nil \|\| true)|./$G/"
  "P4|$G/parse.go|if prev, ok := b.Lair[who]; !ok \|\| c.CreatedAt >= prev.At {|if _, ok := b.Lair[who]; !ok {|./$G/"
  "P5|$G/parse.go|if *dst == \"\" {|if true {|./$G/"
  "P6|$G/parse.go|m := ms[len(ms)-1]|m := ms[0]|./$G/"
  "R1|$G/ghcache.go|if !moved[k] && (e.MarkAt.IsZero() \|\| !now.Before(e.MarkAt.Add(every))) {|if !moved[k] {|./$G/"
  "R2|$G/ghcache.go|if h, ok := heads[n]; ok && h != e.Mark.Head {|if false {|./$G/"
  "R3|$G/ghcache.go|case ok && (e.DeepAt.IsZero() \|\| m != e.Mark \|\| e.Unsettled):|case ok:|./$G/"
  "R4|$G/ghcache.go|case !now.Before(e.EndedAt.Add(p.ClosedTTL)):|case false:|./$G/"
  "R5|$G/ghcache.go|e.Unsettled = pr.State == \"OPEN\" && (pr.Mergeable == \"UNKNOWN\" \|\| pr.MergeSt == \"UNKNOWN\")|e.Unsettled = false|./$G/"
  "R6|$G/ghcache.go|if size() <= MaxForm {|if true {|./$G/"
  "R7|$G/query.go|const DeepBatch = 10|const DeepBatch = 11|./$G/"
  "R8|$G/config.go|HotEvery  = 5 * time.Minute|HotEvery  = 4 * time.Minute|./$G/"
  "R9|$G/config.go|if !termRe.MatchString(t) {|if false {|./$G/"
)
bad=0
for m in "${MUTANTS[@]}"; do
  IFS='|' read -r id file old new pkg <<<"$(sed 's/\\|/\x01/g' <<<"$m")"
  old=${old//$'\x01'/|}; new=${new//$'\x01'/|}
  cp -p "$file" "$BK/orig" || exit 2
  if ! python3 - "$file" "$old" "$new" <<'EOF'
import sys
p, o, n = sys.argv[1:]
s = open(p).read()
if s.count(o) != 1:
    sys.exit(3)
open(p, "w").write(s.replace(o, n))
EOF
  then echo "$id DIDNT-APPLY"; bad=1; continue; fi
  mod=.; case $pkg in ./plugins/github/*) mod=plugins/github; pkg=./${pkg#./plugins/github/};; esac
  out=$(cd "$mod" && go test -count=1 "$pkg" 2>&1); st=$?
  cp -p "$BK/orig" "$file" && cmp -s "$BK/orig" "$file" || { echo "$file not restored" >&2; exit 2; }
  if grep -q '\[build failed\]\|cannot use\|undefined:' <<<"$out"; then echo "$id DIDNT-COMPILE"; bad=1
  elif [ $st -eq 0 ]; then echo "$id NOT-CAUGHT"; bad=1
  else echo "$id KILLED $(grep -o -- '--- FAIL: [A-Za-z0-9_]*' <<<"$out" | head -3 | sed 's/--- FAIL: //' | paste -sd,)"; fi
done
n=$(echo "${#MUTANTS[@]}"); [ "$n" -ge 18 ] || { echo "only $n mutants" >&2; exit 2; }
exit $bad
