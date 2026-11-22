#!/usr/bin/env bash
# ghcache-mutants.sh: run the ghcache mutants (slice 1, and slice 3: S*) (testing standard 4.3). For a build node, never a seat's own machine.
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
  "R1|$G/ghcache.go|if !moved[k] && (e.MarkAt.IsZero() \|\| !now.Before(e.MarkAt.Add(every))) {|if !moved[k] && (true \|\| e.MarkAt.IsZero() \|\| !now.Before(e.MarkAt.Add(every))) {|./$G/"
  "R2|$G/ghcache.go|if h, ok := heads[n]; ok && h != e.Mark.Head {|if h, ok := heads[n]; ok && h != e.Mark.Head && false {|./$G/"
  "R3|$G/ghcache.go|(ok && (e.DeepAt.IsZero() \|\| m != e.Mark \|\| e.Unsettled))|(ok && (true \|\| m != e.Mark))|./$G/"
  "R4|$G/ghcache.go|case !now.Before(e.EndedAt.Add(p.ClosedTTL)):|case false:|./$G/"
  "R5|$G/ghcache.go|e.Unsettled = pr.State == \"OPEN\" && (pr.Mergeable == \"UNKNOWN\" \|\| pr.MergeSt == \"UNKNOWN\")|e.Unsettled = false|./$G/"
  "R6|$G/ghcache.go|if size() <= MaxForm {|if size() >= 0 {|./$G/"
  "R7|$G/query.go|const DeepBatch = 10|const DeepBatch = 11|./$G/"
  "R8|$G/config.go|HotEvery  = 5 * time.Minute|HotEvery  = 4 * time.Minute|./$G/"
  "N1|$G/ghcache.go|if len(e.Path) == 2 && e.Type == \"NOT_FOUND\" {|if len(e.Path) == 2 && e.Type == \"NOT_FOUND\" && false {|./$G/"
  "N2|$G/ghcache.go|p.Config.Max; over > 0 {|p.Config.Max; over > 1000 {|./$G/"
  "N3|$G/ghcache.go|if p.Landed < len(prev.Wrote) {|if p.Landed < len(prev.Wrote) && false {|./$G/"
  "N4|$G/ghcache.go|for k := range p.Children {|for k := range map[string]string{} {|./$G/"
  "N5|$G/ghcache.go|if c.want > c.got {|if c.want > c.got && false {|./$G/"
  "N6|$G/config.go|if c.Max < 1 \|\| c.Max > MaxMax {|if c.Max < 1 {|./$G/"
  "N7|sdk/forge/gh.go|return data, out, nil|return data, nil, nil|./sdk/forge/"
  "N8|$G/ghcache.go|e.Digest = \"\" // what was sent never landed|_ = e.Digest // what was sent never landed|./$G/"
  "R9|$G/config.go|if !termRe.MatchString(t) {|if false {|./$G/"
  # Slice 3 (write path, trusted write scope).
  "S1|$G/sync.go|return s.p.Auth(ent, field)|return s.p.Auth(ent, field)[:0]|./$G/"
  "S2|$G/sync.go|case fr.labelsTotal > len(fr.labels) \|\| !slices.Equal(sortedSet(have), gl):|case false:|./$G/"
  "S3|$G/sync.go|case fr.draft != G.Draft:|case false:|./$G/"
  "S4|$G/ghcache.go|case e.Pend != nil && !unlanded[k]:|case e.Pend != nil \|\| unlanded[k]:|./$G/"
  "S5|$G/sync.go|return // one wake per alarm|_ = 0 // one wake per alarm|./$G/"
  "S6|$G/sync.go|if len(s.raised) > MaxAlarmPings {|if false {|./$G/"
  "S7|$G/sync.go|case slices.Contains(t.mine, r.body):|case false:|./$G/"
  "S8|$G/sync.go|case slices.Contains(e.Sent, h):|case false:|./$G/"
  "S9|$G/sync.go|} else if why := disclose.Disclosure(r.Body); why != \"\" {|} else if why := disclose.Disclosure(r.Body)[:0]; why != \"\" {|./$G/"
  "S10|$G/sync.go|if e.RO != \"\" && roHash(F) != e.RO {|if false {|./$G/"
  "S11|$G/ghcache.go|fw.Expect[\"gh\"] = fv.ghRaw|_ = fv.ghRaw|./$G/"
  "S12|$G/ghcache.go|fw.Expect[\"sync\"] = fv.syncRaw|_ = fv.syncRaw|./$G/"
  "S13|$G/sync.go|const MaxTries = 2|const MaxTries = 3|./$G/"
  "S14|$G/ghcache.go|case fv.gh != nil && e.Base == nil:|case false:|./$G/"
  "S15|$G/sync.go|case pr.Labels.TotalCount > len(pr.Labels.Nodes):|case false:|./$G/"
  "S16|$G/sync.go|if s.writes+nWrites > MaxWrites \|\| !nodeIDRe.MatchString(pr.ID) {|if !nodeIDRe.MatchString(pr.ID) {|./$G/"
  "S17|$G/sync.go|case gt.Resolved && !bt:|case false:|./$G/"
  "S18|$G/ghcache.go|delete(e.Alarms, key)|_ = key|./$G/"
  "S19|$G/sync.go|delete(e.Alarms, AlarmWriteRefused+\".\"+field)|_ = field|./$G/"
  "S20|$G/sync.go|if (len(e.Path) >= 2 && e.Path[0] == \"repo\") \|\||if false \|\||./$G/"
  "S21|internal/poll/forms.go|	for k, v := range expect {|	for k, v := range map[string]json.RawMessage{} {|./internal/poll/"
  "S22|internal/poll/forms.go|if w.Maintainer == \"\" \|\| pg.To != w.Maintainer {|if false && pg.To != w.Maintainer {|./internal/poll/"
  "S23|internal/poll/outbox.go|pr.HeadSHA == \"\" \|\| !slices.Contains(w.ApprovePRs, ref)|pr.HeadSHA == \"\" \|\| !slices.Contains(w.ApprovePRs, ref) && false|./internal/poll/"
  "S24|internal/poll/outbox.go|if w.Approve && (w.DiscoverRepo != \"\" \|\| w.DiscoverAuthor != \"\") {|if false {|./internal/poll/"
  "S25|internal/form/formstate.go|ghConfigIgnored(f.Keys[model.GHCacheConfigKey], w.Maintainer)...)|[]string{}...)|./internal/form/"
  "S26|internal/form/formstate.go|least.Scope, least.WriteFields, least.Maintainer, least.ApprovePRs, least.AutomatedMarkers = nil, nil, \"\", nil, nil|_ = least.Scope|./internal/form/"
  "S27|sdk/model/ghwrite.go|if slices.ContainsFunc(s.ReadOnlyAuthors, func(a string) bool { return strings.EqualFold(a, author) }) {|if false {|./sdk/model/"
  "S28|sdk/model/ghwrite.go|if !slices.Contains(g.Fields, field) {|if false {|./sdk/model/"
  "S29|sdk/model/ghwrite.go|if !g.Scope.IssuesFixedByScope {|if false {|./sdk/model/"
  "S30|sdk/model/authority.go|	w.GHWrite = g|	_ = g|./sdk/model/"
  "S31|sdk/forge/gh.go|		return nil, fmt.Errorf(\"mutation: %s %s\", r.Errors[0].Type, r.Errors[0].Message)|		_ = r.Errors|./sdk/forge/"
  "S32|internal/poll/outbox.go|	if len(bs) == 0 {|	if true {|./internal/poll/"
  "S33|sdk/model/approveblock.go|if r.State == \"CHANGES_REQUESTED\" && !automated(r.Body, markers) {|if r.State == \"CHANGES_REQUESTED\" {|./internal/poll/"
  "S34|sdk/model/approveblock.go|if m != \"\" && strings.Contains(body, m) {|if strings.Contains(body, m) {|./internal/poll/"
  "S35|sdk/model/approveblock.go|if MarksBlocking(c.Body) && !automated(c.Body, markers) {|if false {|./internal/poll/"
  "S36|sdk/model/approveblock.go|return blockingRe.MatchString(notBlockingRe.ReplaceAllString(body, \"\"))|return blockingRe.MatchString(body)|./internal/poll/"
  "S37|sdk/model/approveblock.go|if !pr.Complete {|if false {|./internal/poll/"
  "S38|sdk/model/approveblock.go|r.SubmittedAt > p.SubmittedAt|r.SubmittedAt < p.SubmittedAt|./internal/poll/"
  "S39|sdk/model/approveblock.go|if t.IsResolved {|if false {|./internal/poll/"
  "S40|sdk/model/authority.go|w.ApprovePRs, w.AutomatedMarkers = slices.Clone(a.ApprovePRs), slices.Clone(a.AutomatedMarkers)|w.ApprovePRs = slices.Clone(a.ApprovePRs)|./sdk/model/"
  "S41|sdk/model/authority.go|	set(\"maintainer\", w.Maintainer, a.Maintainer, &w.Maintainer)|	_ = a.Maintainer|./sdk/model/"
  "S42|plugins/github/internal/github/post.go|	if bs := model.ApproveBlockers(fresh, w.AutomatedMarkers); len(bs) > 0 {|	if bs := model.ApproveBlockers(authorized, w.AutomatedMarkers); len(bs) > 0 {|./plugins/github/internal/github/"
  "S43|plugins/github/internal/github/post.go|	case fresh.HeadSHA != authorized.HeadSHA:|	case false:|./plugins/github/internal/github/"
  "S44|plugins/github/internal/github/post.go|	case fresh == nil \|\| !fresh.Complete:|	case fresh == nil:|./plugins/github/internal/github/"
  "S45|internal/poll/outbox.go|	return pluginBlocked(w, snap, order, todo, alarms, now)|	return nil|./internal/poll/"
  "S46|internal/poll/outbox.go|!strings.HasPrefix(post.Refused, model.ApproveBlocked) \|\| alarms[id] != nil {|alarms[id] != nil {|./internal/poll/"
  "S47|$G/ghcache.go|			b := e.Pend.Pushed.onto(*e.Base)|			b := *e.Base|./$G/"
  "S48|$G/ghcache.go|							fw.ExpectAbsent = append(fw.ExpectAbsent, \"gh\")|							_ = fw|./$G/"
  "S49|$G/ghcache.go|							fw.ExpectAbsent = append(fw.ExpectAbsent, \"sync\")|							_ = fw|./$G/"
  "S50|internal/poll/forms.go|	for _, k := range absent {|	for _, k := range absent[:0] {|./internal/poll/"
  "S51|$G/sync.go|				e.pushed().Added, e.pushed().Removed = slices.Clone(addL), slices.Clone(rmL)|				_, _ = addL, rmL|./$G/"
  "S52|$G/sync.go|		b.Labels = sortedSet(append(minus(b.Labels, p.Removed), p.Added...))|		b.Labels = sortedSet(append(slices.Clone(b.Labels), p.Added...))|./$G/"
)
# Baseline: every package a mutant is judged by must be green unmutated, or a KILLED proves nothing (job 20999:
# two red tests "killed" every mutant).
for pk in ./internal/poll/ ./internal/pluginrun/ ./internal/form/ ./$G/ ./plugins/github/internal/github/ ./sdk/forge/ ./sdk/model/; do
  mod=.; p=$pk
  case $pk in ./plugins/github/*) mod=plugins/github; p=./${pk#./plugins/github/};; ./sdk/*) mod=sdk; p=./${pk#./sdk/};; esac
  if ! out=$(cd "$mod" && go test -count=1 "$p" 2>&1); then
    echo "baseline red: $pk"; grep -- '--- FAIL' <<<"$out" | head -5; exit 2
  fi
done
echo "baseline green"
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
  mod=.; case $pkg in ./plugins/github/*) mod=plugins/github; pkg=./${pkg#./plugins/github/};; ./sdk/*) mod=sdk; pkg=./${pkg#./sdk/};; esac
  out=$(cd "$mod" && go test -count=1 "$pkg" 2>&1); st=$?
  cp -p "$BK/orig" "$file" && cmp -s "$BK/orig" "$file" || { echo "$file not restored" >&2; exit 2; }
  if grep -q '\[build failed\]\|cannot use\|undefined:' <<<"$out"; then echo "$id DIDNT-COMPILE"; bad=1
  elif [ $st -eq 0 ]; then echo "$id NOT-CAUGHT"; bad=1
  else echo "$id KILLED $(grep -o -- '--- FAIL: [A-Za-z0-9_]*' <<<"$out" | head -3 | sed 's/--- FAIL: //' | paste -sd,)"; fi
done
n=$(echo "${#MUTANTS[@]}"); [ "$n" -ge 78 ] || { echo "only $n mutants" >&2; exit 2; }
exit $bad
