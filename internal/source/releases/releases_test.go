package releases

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gluck/ebac/internal/model"
	"github.com/gluck/ebac/internal/plugin"
	"github.com/gluck/ebac/internal/trigger"
)

// figaro's real recent tags (2026-11-09), newest first as the API returns them.
var figaroTags = []string{"v0.58.4", "v0.58.3", "v0.58.2", "v0.58.1", "v0.58.0", "v0.57.5"}

func rels(tags ...string) []Release {
	out := make([]Release, len(tags))
	for i, t := range tags {
		out[i] = Release{Tag: t, URL: "https://github.com/jack-work/figaro/releases/tag/" + t, Body: "notes for " + t, Published: "2026-11-09T00:00:00Z"}
		out[i].Author.Login = "jack-work"
	}
	return out
}

// The order's proof: replaying the last three figaro tags yields three events with the right bump class.
func TestReplayLastThreeTags(t *testing.T) {
	st := &model.ReleasesState{}
	if news := Diff(st, rels("v0.57.5")); len(news) != 0 {
		t.Fatalf("the first round must only record: %v", news)
	}
	news := Diff(st, rels("v0.58.2", "v0.58.1", "v0.58.0", "v0.57.5"))
	var got []string
	for _, n := range news {
		got = append(got, n.Line())
	}
	want := []string{"v0.58.0 minor (from v0.57.5)", "v0.58.1 patch (from v0.58.0)", "v0.58.2 patch (from v0.58.1)"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("replay:\n got %v\nwant %v", got, want)
	}
	// Control: the same list again is no news.
	if news := Diff(st, rels("v0.58.2", "v0.58.1", "v0.58.0", "v0.57.5")); len(news) != 0 {
		t.Fatalf("a seen list announced %v", news)
	}
	// A major, and a first release of a repo armed with none.
	if news := Diff(st, rels("v1.0.0", "v0.58.2")); len(news) != 1 || news[0].Bump != "major" || news[0].Previous != "v0.58.2" {
		t.Fatalf("major: %+v", news)
	}
	empty := &model.ReleasesState{}
	Diff(empty, nil)
	if news := Diff(empty, rels("v0.1.0")); len(news) != 1 {
		t.Fatalf("a repo armed with no release must announce its first: %v", news)
	}
}

func TestBumpAndOrder(t *testing.T) {
	for _, c := range [][3]string{
		{"v0.58.3", "v0.58.4", "patch"}, {"v0.57.5", "v0.58.0", "minor"}, {"v0.58.4", "v1.0.0", "major"},
		{"v1.2.3", "v1.2.4-rc1", "patch"}, {"", "v0.1.0", "major"}, {"v1.0.0", "nightly", "unknown"},
	} {
		if got := Bump(c[0], c[1]); got != c[2] {
			t.Errorf("Bump(%s, %s) = %s; want %s", c[0], c[1], got, c[2])
		}
	}
	if !Less("v0.58.9", "v0.58.10") || !Less("v1.0.0-rc1", "v1.0.0") || Less("v1.0.0", "v1.0.0") {
		t.Fatal("semver order")
	}
}

// Through the wire, against a fake api.github.com: new tags are wake events with inbox items and the releases key;
// a refusal (rate limit) is a failed round with the status in its error.
func TestPluginRound(t *testing.T) {
	status := http.StatusOK
	list := rels(figaroTags[:4]...)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/jack-work/figaro/releases" || r.Header.Get("Authorization") != "" {
			http.Error(w, "unexpected "+r.URL.Path, 400)
			return
		}
		if status != http.StatusOK {
			http.Error(w, `{"message":"API rate limit exceeded"}`, status)
			return
		}
		json.NewEncoder(w).Encode(list)
	}))
	t.Cleanup(srv.Close)
	w := model.Critic{Name: "releases-figaro", FormID: "@r", Source: model.SourceReleases,
		Releases: &model.ReleasesRef{Repo: "jack-work/figaro", AlertRole: "@335d516c", API: srv.URL}}
	w.Runtime.Releases = &model.ReleasesState{Armed: true, Seen: figaroTags[1:]}
	call := func(c model.Critic) *plugin.Reply {
		t.Helper()
		rep, err := plugin.InProc{"gh": Plugin{HTTP: srv.Client()}.Handle}.Call(context.Background(), "gh", plugin.Request{Op: plugin.OpRound, Critic: c})
		if err != nil {
			t.Fatal(err)
		}
		return rep
	}
	rep := call(w)
	if rep.Error != "" || len(rep.Events) != 1 || rep.Events[0].Detail != "v0.58.4 patch (from v0.58.3)" || rep.Events[0].Tier != model.TierWake {
		t.Fatalf("round: error %q events %+v", rep.Error, rep.Events)
	}
	if it, ok := rep.Inbox["release-v0.58.4"]; !ok || it.Body != "notes for v0.58.4" || rep.Events[0].Inbox[0] != "release-v0.58.4" {
		t.Fatalf("inbox: %+v", rep.Inbox)
	}
	if !strings.Contains(string(rep.Set["releases"]), "v0.58.4 patch") {
		t.Fatalf("releases key: %s", rep.Set["releases"])
	}
	if got := (&model.Critic{Releases: w.Releases, FormID: "@r"}).AlertTarget(); got != "@335d516c" {
		t.Fatalf("alert target %s", got)
	}
	status = http.StatusForbidden
	if rep := call(w); !strings.Contains(rep.Error, "HTTP 403") || !strings.Contains(rep.Error, "rate limit") || rep.Critic.Runtime.ConsecutiveFailures != 1 {
		t.Fatalf("refusal: %q failures %d", rep.Error, rep.Critic.Runtime.ConsecutiveFailures)
	}
	// Every release wakes by default; the brief names the tag and its bump.
	if d := trigger.DefaultFor(model.SourceReleases); len(d) != 1 || d[0].Action != trigger.Wake {
		t.Fatal("default rules: every release must wake")
	}
	b, err := plugin.InProc{"gh": Plugin{}.Handle}.Call(context.Background(), "gh", plugin.Request{Op: plugin.OpBrief, Critic: w, Pending: rep.Events})
	if err != nil || !strings.Contains(b.Brief, "v0.58.4 patch (from v0.58.3)") {
		t.Fatalf("brief %+v %v", b, err)
	}
}
