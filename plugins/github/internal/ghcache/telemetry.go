package ghcache

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gluck/ebac/sdk/plugin"
)

// Telemetry is one round's record: what was read and why, what was written,
// what it cost. The plugin returns it in Reply.Telemetry; the core appends it
// to <state>/telemetry/<critic>.jsonl, so it outlives the journal.
type Telemetry struct {
	At      string `json:"at"`
	Tracked int    `json:"tracked"`
	Due     int    `json:"due"`
	Search  bool   `json:"search"`
	// Searched are the PRs the searches answered; AnsweredTwice those also
	// asked by their own alias in the same marks call.
	Searched      int        `json:"searched"`
	AnsweredTwice int        `json:"answered_twice"`
	Deep          []DeepRec  `json:"deep"`
	FormsWritten  int        `json:"forms_written"`
	Retired       int        `json:"retired"`
	Writes        []WriteRec `json:"writes,omitempty"`
	Alarms        []string   `json:"alarms,omitempty"` // "<key> <kind>.<field>"
	GraphQL       *Cost      `json:"graphql,omitempty"`
	RESTCalls     int        `json:"rest_calls"`
	Error         string     `json:"error,omitempty"` // the round failed
	Metrics       []Metric   `json:"metrics,omitempty"`
}

// DeepRec is one PR read in full and why: first (never read deep), head (a
// head moved over ls-refs), mark (its mark moved: Moved names the fields),
// unknown-backoff (the hourly re-read of UNKNOWN mergeability), or a redo
// reason (retry, unlanded, unminted, ack, unparseable, no-base,
// pending-write, form-edit).
type DeepRec struct {
	PR    int      `json:"pr"`
	Repo  string   `json:"repo"`
	Why   string   `json:"why"`
	Moved []string `json:"moved,omitempty"`
}

// WriteRec is one GitHub write.
type WriteRec struct {
	PR       string `json:"pr"`
	Mutation string `json:"mutation"`
	OK       bool   `json:"ok"`
	Error    string `json:"error,omitempty"`
}

// Cost is the round's GraphQL spend.
type Cost struct {
	Calls  int `json:"calls"`
	Points int `json:"points"`
	Left   int `json:"left"`
}

// deepWhy names why k is read deep this round, in the order the trigger is
// tested (a moved head first, then a redo, then the marks).
func deepWhy(k string, e *Entity, headMoved bool, redo string, marked bool, m Mark, stuck bool) DeepRec {
	d := DeepRec{PR: e.N, Repo: e.Repo}
	switch {
	case headMoved:
		d.Why = "head"
	case e.Redo:
		d.Why = redo
		if d.Why == "" {
			d.Why = "retry"
		}
	case e.DeepAt.IsZero():
		d.Why = "first"
	case marked && m != e.Mark:
		d.Why, d.Moved = "mark", markMoved(e.Mark, m)
	case stuck:
		d.Why = "unknown-backoff"
	default:
		d.Why = "unknown"
	}
	return d
}

// markMoved names the mark fields that differ.
func markMoved(a, b Mark) []string {
	var out []string
	for _, f := range []struct {
		name string
		x, y any
	}{
		{"head", a.Head, b.Head}, {"updated_at", a.UpdatedAt, b.UpdatedAt}, {"state", a.State, b.State},
		{"draft", a.Draft, b.Draft}, {"rollup", a.Rollup, b.Rollup}, {"mergeable", a.Mergeable, b.Mergeable},
		{"merge_state", a.MergeState, b.MergeState}, {"checks", a.Checks, b.Checks},
	} {
		if f.x != f.y {
			out = append(out, f.name)
		}
	}
	return out
}

var mutationRe = regexp.MustCompile(`mutation[^{]*\{\s*([A-Za-z]+)`)

// mutationName is the first field of a mutation (its operation).
func mutationName(q string) string {
	if m := mutationRe.FindStringSubmatch(q); m != nil {
		return m[1]
	}
	return "unknown"
}

// finish fills the round's totals. A PR asked by its alias while a search
// also answered it (inSearch) is answered twice in the one marks call.
func (t *Telemetry) finish(now time.Time, tracked, due int, search bool, inSearch map[string]bool, dueRefs []Ref,
	forms []plugin.FormWrite, s *syncer) {
	t.At = now.Format(time.RFC3339)
	t.Tracked, t.Due, t.Search, t.Searched = tracked, due, search, len(inSearch)
	for _, ref := range dueRefs {
		if inSearch[ChildKey(ref.Repo, ref.N)] {
			t.AnsweredTwice++
		}
	}
	for _, f := range forms {
		if f.Retire {
			t.Retired++
		} else {
			t.FormsWritten++
		}
	}
	t.Writes = s.log
	for _, r := range s.raised {
		k, rest, _ := strings.Cut(r, " ")
		alarm, _, _ := strings.Cut(rest, ":")
		t.Alarms = append(t.Alarms, k+" "+alarm)
	}
	sort.Slice(t.Deep, func(i, j int) bool {
		if t.Deep[i].Repo != t.Deep[j].Repo {
			return t.Deep[i].Repo < t.Deep[j].Repo
		}
		return t.Deep[i].PR < t.Deep[j].PR
	})
}

// Metric is one value of the round for the core's OTLP export: a counter's
// Value is this round's increment, a gauge's its current value.
type Metric struct {
	Name  string            `json:"name"`
	Kind  string            `json:"kind"`
	Value float64           `json:"value"`
	Attrs map[string]string `json:"attrs,omitempty"`
}

// metrics are the round's series (names: ebac_ghcache_*), set on the record
// last, once the cost is known.
func (t *Telemetry) metrics() {
	var ms []Metric
	add := func(name, kind string, v float64, attrs map[string]string) {
		ms = append(ms, Metric{Name: name, Kind: kind, Value: v, Attrs: attrs})
	}
	outcome := "ok"
	if t.Error != "" {
		outcome = "error"
	}
	add("ebac_ghcache_rounds_total", "counter", 1, map[string]string{"outcome": outcome})
	why, fields := map[string]int{}, map[string]int{}
	for _, d := range t.Deep {
		why[d.Why]++
		for _, f := range d.Moved {
			fields[f]++
		}
	}
	for _, w := range sortedKeys(why) {
		add("ebac_ghcache_deep_reads_total", "counter", float64(why[w]), map[string]string{"why": w})
	}
	for _, f := range sortedKeys(fields) {
		add("ebac_ghcache_mark_moves_total", "counter", float64(fields[f]), map[string]string{"field": f})
	}
	writes := map[[2]string]int{}
	for _, w := range t.Writes {
		writes[[2]string{w.Mutation, strconv.FormatBool(w.OK)}]++
	}
	for k, n := range writes {
		add("ebac_ghcache_writes_total", "counter", float64(n), map[string]string{"mutation": k[0], "ok": k[1]})
	}
	kinds := map[string]int{}
	for _, a := range t.Alarms {
		_, ka, _ := strings.Cut(a, " ")
		kind, _, _ := strings.Cut(ka, ".")
		kinds[kind]++
	}
	for _, k := range sortedKeys(kinds) {
		add("ebac_ghcache_alarms_total", "counter", float64(kinds[k]), map[string]string{"kind": k})
	}
	add("ebac_ghcache_forms_written_total", "counter", float64(t.FormsWritten), nil)
	add("ebac_ghcache_answered_twice_total", "counter", float64(t.AnsweredTwice), nil)
	if t.GraphQL != nil {
		add("ebac_ghcache_graphql_points_total", "counter", float64(t.GraphQL.Points), nil)
		add("ebac_ghcache_graphql_calls_total", "counter", float64(t.GraphQL.Calls), nil)
		if t.GraphQL.Left >= 0 {
			add("ebac_ghcache_graphql_points_left", "gauge", float64(t.GraphQL.Left), nil)
		}
	}
	add("ebac_ghcache_rest_calls_total", "counter", float64(t.RESTCalls), nil)
	add("ebac_ghcache_tracked", "gauge", float64(t.Tracked), nil)
	add("ebac_ghcache_due", "gauge", float64(t.Due), nil)
	sort.SliceStable(ms, func(i, j int) bool { return ms[i].Name+fmt.Sprint(ms[i].Attrs) < ms[j].Name+fmt.Sprint(ms[j].Attrs) })
	t.Metrics = ms
}

func sortedKeys(m map[string]int) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

// String is one summary line (for tests and logs).
func (t Telemetry) String() string {
	byWhy := map[string]int{}
	for _, d := range t.Deep {
		byWhy[d.Why]++
	}
	var ks []string
	for k, n := range byWhy {
		ks = append(ks, fmt.Sprintf("%s=%d", k, n))
	}
	sort.Strings(ks)
	return fmt.Sprintf("deep %d (%s)", len(t.Deep), strings.Join(ks, " "))
}
