package orgapprove

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gluck/ebac/internal/model"
)

// leanFields: scalars and one totalCount, no comment bodies (1 point for 14
// aliases measured 2026-11-17).
const leanFields = `... on PullRequest{ number title createdAt isDraft headRefOid author{login} }`

// LeanPR is one search hit.
type LeanPR struct {
	Number     int    `json:"number"`
	Title      string `json:"title"`
	CreatedAt  string `json:"createdAt"`
	IsDraft    bool   `json:"isDraft"`
	HeadRefOid string `json:"headRefOid"`
	Author     struct {
		Login string `json:"login"`
	} `json:"author"`
}

// LeanQuery is the round's one search query: an alias per login.
func LeanQuery(logins []string) string {
	var b strings.Builder
	b.WriteString("query{")
	for i, l := range logins {
		q, _ := json.Marshal(fmt.Sprintf("repo:%s is:pr is:open author:%s", Repo, l))
		fmt.Fprintf(&b, " u%d: search(type:ISSUE, first:50, query:%s){nodes{%s}}", i, q, leanFields)
	}
	b.WriteString(" }")
	return b.String()
}

// lean reads every open PR of the logins, Jack's own excluded, by number.
func lean(ctx context.Context, rd Reader, logins []string) ([]LeanPR, error) {
	var ok []string
	for _, l := range logins {
		if ValidLogin(l) && l != Self {
			ok = append(ok, l)
		}
	}
	if len(ok) == 0 {
		return nil, fmt.Errorf("no logins to read")
	}
	data, err := rd.Query(ctx, LeanQuery(ok))
	if err != nil {
		return nil, fmt.Errorf("lean search: %w", err)
	}
	var d map[string]struct {
		Nodes []LeanPR `json:"nodes"`
	}
	if err := json.Unmarshal(data, &d); err != nil {
		return nil, fmt.Errorf("lean search: decode: %w", err)
	}
	byN := map[int]LeanPR{}
	for i := range ok {
		for _, n := range d["u"+strconv.Itoa(i)].Nodes {
			if n.Number > 0 && n.Author.Login != Self {
				byN[n.Number] = n
			}
		}
	}
	out := make([]LeanPR, 0, len(byN))
	for _, n := range byN {
		out = append(out, n)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Number < out[j].Number })
	return out, nil
}

// cheapFails are the criteria the lean read already decides.
func cheapFails(l LeanPR, now time.Time) []string {
	var f []string
	if l.IsDraft {
		f = append(f, "not-draft")
	}
	if t, err := time.Parse(time.RFC3339, l.CreatedAt); err != nil || now.Sub(t) < MinAge {
		f = append(f, "age>=2d")
	}
	return f
}

const deepFields = `number isDraft headRefOid reviewDecision mergeable author{login}
 commits{totalCount}
 force: timelineItems(itemTypes:[HEAD_REF_FORCE_PUSHED_EVENT]){totalCount}
 last: commits(last:1){nodes{commit{statusCheckRollup{state contexts(first:60){nodes{__typename ... on CheckRun{name conclusion} ... on StatusContext{context state}}}}}}}
 reviews(last:40){nodes{state author{login __typename}}}
 reviewThreads(first:60){totalCount nodes{isResolved first: comments(first:1){nodes{author{login __typename}}} lastc: comments(last:1){nodes{author{login __typename}}}}}
 files(first:100){totalCount nodes{path}}`

type actor struct {
	Login    string `json:"login"`
	Typename string `json:"__typename"`
}

type authorNodes struct {
	Nodes []struct {
		Author *actor `json:"author"`
	} `json:"nodes"`
}

// DeepPR is one deep read.
type DeepPR struct {
	Number         int    `json:"number"`
	IsDraft        bool   `json:"isDraft"`
	HeadRefOid     string `json:"headRefOid"`
	ReviewDecision string `json:"reviewDecision"`
	Mergeable      string `json:"mergeable"`
	Author         actor  `json:"author"`
	Commits        struct {
		TotalCount int `json:"totalCount"`
	} `json:"commits"`
	Force struct {
		TotalCount int `json:"totalCount"`
	} `json:"force"`
	Last struct {
		Nodes []struct {
			Commit struct {
				StatusCheckRollup *struct {
					State    string `json:"state"`
					Contexts struct {
						Nodes []struct {
							Name       string `json:"name"`
							Conclusion string `json:"conclusion"`
							Context    string `json:"context"`
							State      string `json:"state"`
						} `json:"nodes"`
					} `json:"contexts"`
				} `json:"statusCheckRollup"`
			} `json:"commit"`
		} `json:"nodes"`
	} `json:"last"`
	Reviews struct {
		Nodes []struct {
			State  string `json:"state"`
			Author *actor `json:"author"`
		} `json:"nodes"`
	} `json:"reviews"`
	ReviewThreads struct {
		TotalCount int `json:"totalCount"`
		Nodes      []struct {
			IsResolved bool        `json:"isResolved"`
			First      authorNodes `json:"first"`
			LastC      authorNodes `json:"lastc"`
		} `json:"nodes"`
	} `json:"reviewThreads"`
	Files struct {
		TotalCount int `json:"totalCount"`
		Nodes      []struct {
			Path string `json:"path"`
		} `json:"nodes"`
	} `json:"files"`
}

// DeepQuery reads nums in one aliased query.
func DeepQuery(nums []int) string {
	owner, name, _ := strings.Cut(Repo, "/")
	var b strings.Builder
	fmt.Fprintf(&b, "query{ repository(owner:%q,name:%q){", owner, name)
	for _, n := range nums {
		fmt.Fprintf(&b, " p%d: pullRequest(number:%d){%s}", n, n, deepFields)
	}
	b.WriteString(" } }")
	return b.String()
}

func deepRead(ctx context.Context, rd Reader, nums []int) (map[int]*DeepPR, error) {
	data, err := rd.Query(ctx, DeepQuery(nums))
	if err != nil {
		return nil, fmt.Errorf("deep read: %w", err)
	}
	var d struct {
		Repository map[string]*DeepPR `json:"repository"`
	}
	if err := json.Unmarshal(data, &d); err != nil {
		return nil, fmt.Errorf("deep read: decode: %w", err)
	}
	out := map[int]*DeepPR{}
	for _, n := range nums {
		if p := d.Repository["p"+strconv.Itoa(n)]; p != nil && p.Number == n {
			out[n] = p
		}
	}
	return out, nil
}

func isBot(a *actor) bool {
	return a != nil && (a.Typename == "Bot" || isBotLogin(a.Login))
}

// score is score.py (the leader's first candidate scan) in Go: six criteria,
// the exclusions (extra) and the sensitive paths.
func score(pr *model.OrgPR, d *DeepPR, knownBreaks []string, now time.Time) {
	var fails, reasons, extra []string
	check := func(name string, ok bool) {
		if !ok {
			fails = append(fails, name)
		}
	}
	// 1-2: the lean read decided age and draft; the deep read re-checks draft.
	check("not-draft", !d.IsDraft)
	reasons = append(reasons, "open >= 2 d")
	// 3: iterations.
	it := d.Commits.TotalCount + d.Force.TotalCount
	check("iter>=3", it >= MinIterations)
	reasons = append(reasons, fmt.Sprintf("%d commits + %d force-pushes", d.Commits.TotalCount, d.Force.TotalCount))
	// 4: bot review present, every bot thread resolved or last answered by
	// the author, no standing bot change request.
	botThreads, openBot := 0, 0
	for _, t := range d.ReviewThreads.Nodes {
		if len(t.First.Nodes) == 0 || !isBot(t.First.Nodes[0].Author) {
			continue
		}
		botThreads++
		answered := len(t.LastC.Nodes) > 0 && t.LastC.Nodes[0].Author != nil && t.LastC.Nodes[0].Author.Login == d.Author.Login
		if !t.IsResolved && !answered {
			openBot++
		}
	}
	botLatest, humanLatest := map[string]string{}, map[string]string{}
	botReviews := 0
	for _, r := range d.Reviews.Nodes {
		if r.Author == nil {
			continue
		}
		if isBot(r.Author) {
			botReviews++
			botLatest[r.Author.Login] = r.State
			continue
		}
		switch r.State {
		case "APPROVED", "CHANGES_REQUESTED", "DISMISSED":
			humanLatest[r.Author.Login] = r.State
		}
	}
	var botCR, humanCR []string
	for l, s := range botLatest {
		if s == "CHANGES_REQUESTED" {
			botCR = append(botCR, l)
		}
	}
	for l, s := range humanLatest {
		if s == "CHANGES_REQUESTED" {
			humanCR = append(humanCR, l)
		}
	}
	sort.Strings(botCR)
	sort.Strings(humanCR)
	check("bot-reviewed+answered", (botThreads > 0 || botReviews > 0) && openBot == 0 && len(botCR) == 0)
	reasons = append(reasons, fmt.Sprintf("bot threads %d (%d unanswered), bot reviews %d", botThreads, openBot, botReviews))
	if len(botCR) > 0 {
		reasons = append(reasons, "standing bot CR: "+strings.Join(botCR, ","))
	}
	if n := len(d.ReviewThreads.Nodes); d.ReviewThreads.TotalCount > n {
		// Unread threads could hide an unanswered bot thread: not ready.
		check("threads-read", false)
		reasons = append(reasons, fmt.Sprintf("threads read %d/%d", n, d.ReviewThreads.TotalCount))
	}
	// 5: checks green, or red only on known breaks.
	state, reds := "NONE", []string{}
	if len(d.Last.Nodes) > 0 && d.Last.Nodes[0].Commit.StatusCheckRollup != nil {
		r := d.Last.Nodes[0].Commit.StatusCheckRollup
		state = r.State
		for _, c := range r.Contexts.Nodes {
			name := c.Name
			if name == "" {
				name = c.Context
			}
			switch {
			case c.Conclusion == "FAILURE" || c.Conclusion == "TIMED_OUT" || c.Conclusion == "CANCELLED",
				c.State == "FAILURE" || c.State == "ERROR":
				reds = append(reds, name)
			}
		}
	}
	green := state == "SUCCESS"
	if !green && state == "FAILURE" && len(reds) > 0 {
		green = true
		for _, n := range reds {
			if !contains(knownBreaks, n) {
				green = false
			}
		}
	}
	check("checks-green", green)
	reasons = append(reasons, "checks "+state)
	if len(reds) > 0 {
		reasons = append(reasons, "red: "+strings.Join(first(reds, 4), ", "))
	}
	// 6: no human change request standing.
	check("no-human-CR", len(humanCR) == 0)
	if len(humanCR) > 0 {
		reasons = append(reasons, "human CR: "+strings.Join(humanCR, ","))
	}
	if d.Mergeable == "CONFLICTING" {
		extra = append(extra, "CONFLICTING")
	}
	if d.ReviewDecision == "APPROVED" {
		extra = append(extra, "already APPROVED")
	}
	if s, ok := humanLatest[Self]; ok {
		extra = append(extra, "Jack already "+s)
	}
	var sens []string
	for _, f := range d.Files.Nodes {
		if sensRe.MatchString(f.Path) {
			sens = append(sens, f.Path)
		}
	}
	if d.Files.TotalCount > len(d.Files.Nodes) {
		sens = append(sens, fmt.Sprintf("(%d files, %d read)", d.Files.TotalCount, len(d.Files.Nodes)))
	}
	pr.Fails = fails
	pr.Score = 6 - countCriteria(fails)
	pr.Extra, pr.Sens, pr.Reasons = extra, first(sens, 3), reasons
}

// countCriteria counts failed criteria among the six (threads-read is a
// guard, folded into bot-reviewed+answered).
func countCriteria(fails []string) int {
	seen := map[string]bool{}
	for _, f := range fails {
		if f == "threads-read" {
			f = "bot-reviewed+answered"
		}
		seen[f] = true
	}
	return len(seen)
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

func first(xs []string, n int) []string {
	if len(xs) > n {
		return xs[:n]
	}
	return xs
}
