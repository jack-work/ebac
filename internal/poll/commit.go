package poll

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/gluck/ebac/sdk/model"
	"github.com/gluck/ebac/sdk/plugin"
)

// Two-phase rounds (#27 design B). A source whose one upstream write must
// never happen twice (an APPROVE) prepares it in OpRound: it records what it
// will do under the round's CommitToken in its critic runtime and replies
// Commit. The core lands that reply; only once the critic is SAVED (the
// durable write) does it call the plugin again, OpCommit with the same token,
// and the plugin acts only on records prepared under that token. A crash or a
// failed save between the two leaves a record whose token no later round
// carries: never acted on.

// CommitSources may ask Commit; any other source's Commit refuses its reply.
var CommitSources = map[string]bool{model.SourceApprove: true, model.SourceOrgApprove: true}

// newCommitToken is 16 random hex digits.
func newCommitToken() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand: " + err.Error())
	}
	return hex.EncodeToString(b)
}

// ValidateCommit refuses Commit from a source outside CommitSources, and from
// a commit reply (one commit per round).
func ValidateCommit(src string, rep *plugin.Reply, op plugin.Op) error {
	switch {
	case !rep.Commit:
		return nil
	case !CommitSources[src]:
		return fmt.Errorf("source %s may not ask Commit: reply refused", src)
	case op == plugin.OpCommit:
		return fmt.Errorf("a commit reply may not ask Commit again: reply refused")
	}
	return nil
}

// checkReply is every check a reply passes before any of it is applied.
func checkReply(w *model.Critic, rep *plugin.Reply, op plugin.Op) error {
	if err := SourceKeysOnly(w.SourceName(), rep); err != nil {
		return err
	}
	if err := ValidateForms(w, rep); err != nil {
		return err
	}
	if err := ValidatePings(w, rep); err != nil {
		return err
	}
	return ValidateCommit(w.SourceName(), rep, op)
}

// phases are the steps twoPhase drives (the Poller's, or a test's).
type phases struct {
	apply func(*plugin.Reply) (saved bool, err error)
	call  func(plugin.Request) (*plugin.Reply, error)
	check func(*plugin.Reply) error
}

// twoPhase lands rep; when it asked Commit and its critic was saved, it calls
// the plugin with OpCommit (same request, same token), checks that reply and
// lands it too. No save, a dry run, or a refused commit reply: no commit
// (what was prepared is never acted on). The round's error joins both.
func twoPhase(req plugin.Request, rep *plugin.Reply, dry bool, ph phases) error {
	saved, err := ph.apply(rep)
	if !rep.Commit || dry {
		return err
	}
	if !saved {
		return errors.Join(err, fmt.Errorf("commit skipped: the prepared reply was not saved"))
	}
	creq := req
	creq.Op = plugin.OpCommit
	crep, cerr := ph.call(creq)
	if cerr == nil {
		cerr = ph.check(crep)
	}
	if cerr != nil {
		return errors.Join(err, fmt.Errorf("commit: %w", cerr))
	}
	_, aerr := ph.apply(crep)
	return errors.Join(err, aerr)
}
