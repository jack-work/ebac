// Package releases watches one GitHub repo's releases: one event per new
// release tag, with its semver bump against the release before it (W7 item 3,
// for the figaro steward: patch = take + bounce; minor or major = ask the Boss).
//
// It is the github-releases half of ebac-gh. It reads the public REST API
// without a credential (no github.com token exists for ebac; cheroot
// 2026-11-09: unauthenticated api.github.com is an egress allowlist line).
package releases

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gluck/ebac/internal/form"
	"github.com/gluck/ebac/internal/model"
	"github.com/gluck/ebac/internal/plugin"
)

const (
	EvRelease model.EventKind = "release_new"

	DefaultAPI   = "https://api.github.com"
	perPage      = 30
	seenMax      = 60
	notesBytes   = 1500
	viewReleases = 10
)

// Release is one GitHub release, reduced.
type Release struct {
	Tag       string `json:"tag_name"`
	URL       string `json:"html_url"`
	Body      string `json:"body"`
	Draft     bool   `json:"draft"`
	Pre       bool   `json:"prerelease"`
	Published string `json:"published_at"`
	Author    struct {
		Login string `json:"login"`
	} `json:"author"`
}

// Plugin serves github-releases. HTTP, when set, replaces the client (tests).
type Plugin struct {
	HTTP *http.Client
}

func (pl Plugin) Handle(ctx context.Context, req plugin.Request) (*plugin.Reply, error) {
	w := &req.Critic
	if w.Releases == nil || w.Releases.Repo == "" {
		return nil, fmt.Errorf("%s: release watch without a repo", w.Name)
	}
	switch req.Op {
	case plugin.OpRound:
		return pl.round(ctx, w), nil
	case plugin.OpBrief:
		return &plugin.Reply{Brief: brief(w, plugin.Unwire(req.Pending))}, nil
	}
	return nil, fmt.Errorf("op %q not supported", req.Op)
}

func (pl Plugin) fetch(ctx context.Context, ref *model.ReleasesRef) ([]Release, error) {
	api := ref.API
	if api == "" {
		api = DefaultAPI
	}
	cl := pl.HTTP
	if cl == nil {
		cl = &http.Client{Timeout: 30 * time.Second} // the default transport honours HTTPS_PROXY (the egress proxy)
	}
	r, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("%s/repos/%s/releases?per_page=%d", strings.TrimRight(api, "/"), ref.Repo, perPage), nil)
	if err != nil {
		return nil, err
	}
	r.Header.Set("Accept", "application/vnd.github+json")
	r.Header.Set("User-Agent", "ebac")
	resp, err := cl.Do(r)
	if err != nil {
		return nil, fmt.Errorf("releases %s: %w", ref.Repo, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, fmt.Errorf("releases %s: %w", ref.Repo, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("releases %s: HTTP %d: %.200s", ref.Repo, resp.StatusCode, body)
	}
	var rels []Release
	if err := json.Unmarshal(body, &rels); err != nil {
		return nil, fmt.Errorf("releases %s: %w", ref.Repo, err)
	}
	out := rels[:0]
	for _, x := range rels {
		if !x.Draft && x.Tag != "" {
			out = append(out, x)
		}
	}
	return out, nil
}

func (pl Plugin) round(ctx context.Context, w *model.Critic) *plugin.Reply {
	rep := &plugin.Reply{Critic: w}
	rels, err := pl.fetch(ctx, w.Releases)
	if err != nil {
		w.Runtime.ConsecutiveFailures++
		w.Runtime.LastError = err.Error()
		rep.Error = err.Error()
		return rep
	}
	w.Runtime.ConsecutiveFailures, w.Runtime.LastError = 0, ""
	w.Runtime.LastSuccess = time.Now().UTC()
	if w.Runtime.Releases == nil {
		w.Runtime.Releases = &model.ReleasesState{}
	}
	news := Diff(w.Runtime.Releases, rels)
	rep.Inbox = map[string]form.InboxItem{}
	for _, n := range news {
		notes, cut := model.CutBody(strings.TrimSpace(n.Release.Body), notesBytes)
		key := "release-" + n.Release.Tag
		rep.Inbox[key] = form.InboxItem{Kind: string(EvRelease), By: n.Release.Author.Login, Time: n.Release.Published, Body: notes, Cut: cut,
			Fetch: fmt.Sprintf("curl -s %s/repos/%s/releases/tags/%s | jq -r .body", DefaultAPI, w.Releases.Repo, n.Release.Tag)}
		ev := plugin.Event{Event: model.Event{Kind: EvRelease, Tier: model.TierWake, PR: model.PRKey(w.Releases.Repo),
			Author: n.Release.Author.Login, URL: n.Release.URL, Detail: n.Line()}, Text: n.Release.Body, Inbox: []string{key}}
		rep.Events = append(rep.Events, ev)
	}
	rep.SetKey("releases", view(rels))
	return rep
}

// New is one release not seen before, against the release before it.
type New struct {
	Release  Release
	Previous string
	Bump     string // major | minor | patch | unknown
}

func (n New) Line() string {
	prev := n.Previous
	if prev == "" {
		prev = "none"
	}
	return fmt.Sprintf("%s %s (from %s)", n.Release.Tag, n.Bump, prev)
}

// Diff returns the releases not in st.Seen, oldest first, each against the
// highest release below it, and records them. The first round only records:
// a watch that starts by announcing thirty old releases is noise.
func Diff(st *model.ReleasesState, rels []Release) []New {
	first := !st.Armed
	st.Armed = true
	seen := map[string]bool{}
	for _, t := range st.Seen {
		seen[t] = true
	}
	all := map[string]bool{}
	for _, t := range st.Seen {
		all[t] = true
	}
	for _, r := range rels {
		all[r.Tag] = true
	}
	tags := make([]string, 0, len(all))
	for t := range all {
		tags = append(tags, t)
	}
	sort.Slice(tags, func(i, j int) bool { return Less(tags[i], tags[j]) })

	var news []New
	if !first {
		byTag := map[string]Release{}
		for _, r := range rels {
			byTag[r.Tag] = r
		}
		for i, t := range tags {
			r, ok := byTag[t]
			if !ok || seen[t] {
				continue
			}
			prev := ""
			if i > 0 {
				prev = tags[i-1]
			}
			news = append(news, New{Release: r, Previous: prev, Bump: Bump(prev, t)})
		}
	}
	// Seen: newest first, bounded.
	sort.Slice(tags, func(i, j int) bool { return Less(tags[j], tags[i]) })
	if len(tags) > seenMax {
		tags = tags[:seenMax]
	}
	st.Seen = tags
	return news
}

// view is the releases key: the newest few, one line each.
func view(rels []Release) []string {
	rs := append([]Release(nil), rels...)
	sort.Slice(rs, func(i, j int) bool { return Less(rs[j].Tag, rs[i].Tag) })
	if len(rs) > viewReleases {
		rs = rs[:viewReleases]
	}
	out := make([]string, 0, len(rs))
	for i, r := range rs {
		prev := ""
		if i+1 < len(rs) {
			prev = rs[i+1].Tag
		}
		out = append(out, fmt.Sprintf("%s %s %s %s", r.Tag, Bump(prev, r.Tag), r.Published, r.URL))
	}
	return out
}

type semver struct {
	major, minor, patch int
	pre                 string
	ok                  bool
}

func parse(tag string) semver {
	t := strings.TrimPrefix(strings.TrimPrefix(tag, "v"), "V")
	core, pre, _ := strings.Cut(t, "-")
	core, _, _ = strings.Cut(core, "+")
	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return semver{}
	}
	var n [3]int
	for i, p := range parts {
		v, err := strconv.Atoi(p)
		if err != nil || v < 0 {
			return semver{}
		}
		n[i] = v
	}
	return semver{n[0], n[1], n[2], pre, true}
}

// Bump classifies tag against prev: the highest component that changed.
func Bump(prev, tag string) string {
	a, b := parse(prev), parse(tag)
	switch {
	case !b.ok:
		return "unknown"
	case !a.ok:
		return "major" // nothing to compare with: treat as the largest step
	case a.major != b.major:
		return "major"
	case a.minor != b.minor:
		return "minor"
	default:
		return "patch"
	}
}

// Less orders tags by semver (a prerelease before its release); tags that
// are not semver sort first, by text.
func Less(x, y string) bool {
	a, b := parse(x), parse(y)
	switch {
	case !a.ok && !b.ok:
		return x < y
	case !a.ok:
		return true
	case !b.ok:
		return false
	case a.major != b.major:
		return a.major < b.major
	case a.minor != b.minor:
		return a.minor < b.minor
	case a.patch != b.patch:
		return a.patch < b.patch
	case a.pre == b.pre:
		return false
	case a.pre == "":
		return false
	case b.pre == "":
		return true
	default:
		return a.pre < b.pre
	}
}

func brief(w *model.Critic, events []model.Event) string {
	var b strings.Builder
	fmt.Fprintf(&b, "ebac %s round %d: new release(s) of %s.\n", w.Name, w.Runtime.Round, w.Releases.Repo)
	for _, e := range events {
		if e.Tier == model.TierWake {
			fmt.Fprintf(&b, "- %s %s\n", e.Detail, e.URL)
		}
	}
	fmt.Fprintf(&b, "Release notes are in form %s, key inbox (release-<tag>). When finished: ebac ack --critic %s --round %d\n",
		w.FormID, w.Name, w.Runtime.Round)
	return b.String()
}
