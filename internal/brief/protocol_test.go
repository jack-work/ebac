package brief

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gluck/ebac/internal/model"
)

func protocolCritics() []model.Critic {
	return []model.Critic{
		{Name: "ro", Mode: model.ModeReview, FormID: "@f"},
		{Name: "rw", Mode: model.ModeReview, Write: true, FormID: "@f"},
		{Name: "dev", Mode: model.ModeDevelop, IssueRepo: "acme/ebac", FormID: "@f"},
		{Name: "rev", Mode: model.ModeReviewer, Host: "ghe.example", Approve: true, FormID: "@f",
			Worktree: &model.Worktree{Clone: "/src/widget", Root: "/wt"}},
	}
}

func TestBriefCarriesTheDeltaNotTheProtocol(t *testing.T) {
	t.Setenv("EBAC_SIGNATURE", filepath.Join(t.TempDir(), "absent"))
	pr := &model.PRState{Key: model.MakePRKey("acme", "widget", 7), Number: 7, State: "OPEN", Author: "friend", HeadSHA: "abcdef1234567"}
	events := []model.Event{{Kind: model.EvHeadMoved, Tier: model.TierWake, PR: pr.Key}}
	for _, w := range protocolCritics() {
		w.Runtime.Round = 42
		brief := BuildBrief(&w, snapWith(pr), events, "jack", 0)
		for _, banned := range []string{"BOUNDARY", "YOUR JOB", "HOW TO ACT", "SET THE PROTOCOL", "SIGNAL, DO NOT NARRATE", "SIGNATURE"} {
			if strings.Contains(brief, banned) {
				t.Errorf("%s: brief repeats protocol section %q", w.Name, banned)
			}
		}
		for _, want := range []string{
			"ebac " + w.Name + " round 42: ",
			"  acme/widget#7 OPEN head ",
			"New and edited comments are in your studied form @f, key inbox",
			"`figaro study @f`",
			"When finished: ebac ack --critic " + w.Name + " --round 42",
		} {
			if !strings.Contains(brief, want) {
				t.Errorf("%s: brief lacks %q:\n%s", w.Name, want, brief)
			}
		}
	}
}

func TestProtocolChunksFitTheStudyCut(t *testing.T) {
	t.Setenv("EBAC_SIGNATURE", filepath.Join(t.TempDir(), "absent"))
	pr := &model.PRState{Key: model.MakePRKey("acme", "widget", 7), Number: 7, State: "OPEN", Author: "friend"}
	texts := map[string]string{
		"one line longer than the cut, quoted and multibyte": strings.Repeat(`"—" `, 900) + "\nend\n",
	}
	for _, w := range protocolCritics() {
		texts[w.Name] = ProtocolText(&w, snapWith(pr), "jack")
	}
	if len(texts["rw"]) <= FormValueElisionLimit {
		t.Fatalf("rw protocol is %d bytes; the split is untested below the cut", len(texts["rw"]))
	}
	for name, text := range texts {
		chunks := ProtocolChunks(text)
		if got := strings.Join(chunks, ""); got != text {
			t.Errorf("%s: chunks do not reassemble the protocol (%d vs %d bytes)", name, len(got), len(text))
		}
		for i, c := range chunks {
			if n := jsonLen(c); n > FormValueElisionLimit {
				t.Errorf("%s: chunk %d is %d JSON bytes, over the %d cut", name, i+1, n, FormValueElisionLimit)
			}
		}
		if len(text) > FormValueElisionLimit && len(chunks) < 2 {
			t.Errorf("%s: %d bytes in one chunk", name, len(text))
		}
	}
}

func snapWith(prs ...*model.PRState) *model.Snapshot {
	s := &model.Snapshot{PRs: map[model.PRKey]*model.PRState{}, Complete: true, TakenAt: time.Now()}
	for _, p := range prs {
		if p.Threads == nil {
			p.Threads = map[string]*model.Thread{}
		}
		if p.Reviews == nil {
			p.Reviews = map[string]*model.Review{}
		}
		p.Complete = true
		s.PRs[p.Key] = p
	}
	return s
}
