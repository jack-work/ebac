package poll

import (
	"errors"
	"regexp"
	"strings"
	"testing"

	"github.com/gluck/ebac/sdk/model"
	"github.com/gluck/ebac/sdk/plugin"
)

// recorder drives twoPhase with fakes that log every step in order.
type recorder struct {
	steps    []string
	saved    bool  // what apply reports for the first (prepare) reply
	applyErr error // the first apply's error
	commit   *plugin.Reply
	callErr  error
}

func (r *recorder) phases(w *model.Critic) phases {
	n := 0
	return phases{
		apply: func(rep *plugin.Reply) (bool, error) {
			n++
			if n == 1 {
				r.steps = append(r.steps, "apply prepare")
				if r.saved {
					r.steps = append(r.steps, "saved")
				}
				return r.saved, r.applyErr
			}
			r.steps = append(r.steps, "apply commit")
			return true, nil
		},
		call: func(req plugin.Request) (*plugin.Reply, error) {
			r.steps = append(r.steps, "call "+string(req.Op)+" "+req.CommitToken)
			return r.commit, r.callErr
		},
		check: func(rep *plugin.Reply) error { return checkReply(w, rep, plugin.OpCommit) },
	}
}

var commitReq = plugin.Request{Op: plugin.OpRound, CommitToken: "0123456789abcdef"}

// A reply asking Commit: the plugin is called again with OpCommit and the SAME token, only after the save, and the
// commit reply is applied. RED if the commit call moves before the save or loses the token.
func TestCommitRunsAfterTheSave(t *testing.T) {
	w := &model.Critic{Name: "ap", Source: model.SourceApprove, InputsForm: "@in"}
	r := &recorder{saved: true, commit: &plugin.Reply{Done: true}}
	if err := twoPhase(commitReq, &plugin.Reply{Done: true, Commit: true}, false, r.phases(w)); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(r.steps, " | "); got != "apply prepare | saved | call commit 0123456789abcdef | apply commit" {
		t.Fatalf("steps: %s", got)
	}
}

// The save failed: no commit call (what was prepared is never acted on), and the round says so. RED if the core
// commits after a failed save (mutant M2).
func TestNoCommitAfterAFailedSave(t *testing.T) {
	w := &model.Critic{Name: "ap", Source: model.SourceApprove, InputsForm: "@in"}
	r := &recorder{saved: false, applyErr: errors.New("save critic: figaro down"), commit: &plugin.Reply{}}
	err := twoPhase(commitReq, &plugin.Reply{Done: true, Commit: true}, false, r.phases(w))
	if got := strings.Join(r.steps, " | "); got != "apply prepare" {
		t.Fatalf("steps: %s", got)
	}
	if err == nil || err.Error() != "save critic: figaro down\ncommit skipped: the prepared reply was not saved" {
		t.Fatalf("err: %v", err)
	}
}

// No Commit asked, or a dry run: one apply, no call.
func TestNoCommitWhenNotAskedOrDry(t *testing.T) {
	w := &model.Critic{Name: "ap", Source: model.SourceApprove, InputsForm: "@in"}
	for _, c := range []struct {
		desc   string
		commit bool
		dry    bool
	}{{"not asked", false, false}, {"dry run", true, true}} {
		r := &recorder{saved: true, commit: &plugin.Reply{}}
		if err := twoPhase(commitReq, &plugin.Reply{Commit: c.commit}, c.dry, r.phases(w)); err != nil {
			t.Fatalf("%s: %v", c.desc, err)
		}
		if got := strings.Join(r.steps, " | "); got != "apply prepare | saved" {
			t.Errorf("%s: steps %s", c.desc, got)
		}
	}
}

// A commit reply asking Commit again is refused whole: not applied. RED if the one-commit-per-round check goes.
func TestACommitReplyMayNotAskAgain(t *testing.T) {
	w := &model.Critic{Name: "ap", Source: model.SourceApprove, InputsForm: "@in"}
	r := &recorder{saved: true, commit: &plugin.Reply{Done: true, Commit: true}}
	err := twoPhase(commitReq, &plugin.Reply{Done: true, Commit: true}, false, r.phases(w))
	if got := strings.Join(r.steps, " | "); got != "apply prepare | saved | call commit 0123456789abcdef" {
		t.Fatalf("steps: %s", got)
	}
	if err == nil || err.Error() != "commit: a commit reply may not ask Commit again: reply refused" {
		t.Fatalf("err: %v", err)
	}
}

// Only approve-requests and org-approve may ask Commit: one row per source of the closed set. RED if the allowlist
// widens (mutant M3: any source).
func TestOnlyTwoPhaseSourcesMayAskCommit(t *testing.T) {
	for _, c := range []struct {
		src string
		ok  bool
	}{
		{model.SourceApprove, true}, {model.SourceOrgApprove, true},
		{"github-pr", false}, {"github-author", false}, {model.SourceScout, false}, {model.SourceGHCache, false},
		{model.SourceKeelBranches, false}, {"keel-threads", false}, {"teams-chat", false}, {model.SourceBandHub, false},
		{model.SourceRoster, false}, {model.SourceReleases, false},
	} {
		err := ValidateCommit(c.src, &plugin.Reply{Commit: true}, plugin.OpRound)
		if c.ok && err != nil {
			t.Errorf("%s: refused: %v", c.src, err)
		}
		if !c.ok && (err == nil || err.Error() != "source "+c.src+" may not ask Commit: reply refused") {
			t.Errorf("%s: %v", c.src, err)
		}
		if err := ValidateCommit(c.src, &plugin.Reply{}, plugin.OpRound); err != nil {
			t.Errorf("%s without Commit: %v", c.src, err)
		}
	}
}

// The token is 16 hex digits, fresh each round.
func TestCommitTokens(t *testing.T) {
	a, b := newCommitToken(), newCommitToken()
	if !regexp.MustCompile(`^[0-9a-f]{16}$`).MatchString(a) || a == b {
		t.Fatalf("tokens %q %q", a, b)
	}
}
