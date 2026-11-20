package poll

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync/atomic"

	"github.com/gluck/ebac/internal/identity"
	"github.com/gluck/ebac/sdk/model"
)

// Directory reads what identity needs, once per round or command (pollRound
// resets it), from ONE listing: every live form by name with its
// target-aria, every listed aria's parent (reaped ones too, so a chain walks
// through them), and which arias are reaped. The roster form
// is never read (it is a plain, editable form key). Any read failure fails
// the whole read: no check runs on a partial directory.
func (p *Poller) Directory(ctx context.Context) (identity.Directory, error) {
	if p.dir != nil {
		return *p.dir, nil
	}
	arias, err := p.Fig.ListArias(ctx)
	if err != nil {
		return identity.Directory{}, fmt.Errorf("list arias: %w", err)
	}
	if len(arias) == 0 {
		return identity.Directory{}, fmt.Errorf("list arias: empty listing")
	}
	d := BuildDirectory(arias)
	p.dir = &d
	return d, nil
}

// BuildDirectory is the directory of one listing.
func BuildDirectory(arias []model.Aria) identity.Directory {
	d := identity.Directory{Forms: map[string][]string{}, Holder: map[string]string{}, Parent: map[string]string{},
		Reaped: map[string]bool{}}
	for _, a := range arias {
		if a.ReapedAt > 0 {
			d.Reaped[a.ID] = true
			if a.Kind == "form" {
				continue // a reaped form is no role
			}
		}
		switch a.Kind {
		case "", "conversation":
			d.Parent[a.ID] = a.Parent
		case "form":
			id := a.ID
			if !strings.HasPrefix(id, "@") {
				id = "@" + id
			}
			d.Holder[id] = a.TargetAria
			if a.Name != "" && !strings.Contains(a.Name, "~") {
				d.Forms[a.Name] = append(d.Forms[a.Name], id)
			}
		}
	}
	return d
}

// PostCheck: may caller post for role (holder or a live fork)? form is the
// role form resolved; why the refusal; err a read failure (transient: retry).
func (p *Poller) PostCheck(ctx context.Context, caller, role string) (form, why string, err error) {
	d, err := p.Directory(ctx)
	if err != nil {
		return "", "", err
	}
	form, ok, why := d.MayPost(caller, role)
	if ok {
		why = ""
	}
	return form, why, nil
}

// withReviewCheck is the critic's read keys plus, for a keel review, the
// core's verification (model.KeelReviewCheck under review_check): the review
// key's stamp, and per named reviewer whether its verdict counts. The plugin
// never reads the stamp key. Any failure (no key, a figaro read error) gives
// a check where nothing is valid: the review fails closed.
func (p *Poller) withReviewCheck(ctx context.Context, w *model.Critic) map[string]json.RawMessage {
	raw := w.Reads["review"]
	if len(raw) == 0 || w.SourceName() != "keel-threads" || w.Keel == nil {
		return w.Reads
	}
	keys := make(map[string]json.RawMessage, len(w.Reads)+1)
	for k, v := range w.Reads {
		keys[k] = v
	}
	keys[model.KeelReviewCheckKey] = model.MustJSON(p.reviewCheck(ctx, w, raw, w.Reads["verdict"]))
	return keys
}

// stampKeyMissingLogged: the missing stamp key was reported once this process.
var stampKeyMissingLogged atomic.Bool

func (p *Poller) reviewCheck(ctx context.Context, w *model.Critic, rawCfg, rawVerdicts json.RawMessage) model.KeelReviewCheck {
	var chk model.KeelReviewCheck
	var cfg model.KeelReviewConfig
	if json.Unmarshal(rawCfg, &cfg) != nil {
		return chk
	}
	key, err := identity.LoadKey(identity.KeyPath(), false)
	if err != nil {
		// No key yet is normal until the first ebac review makes one: say so
		// once per process, not every round. Any other failure every time.
		if !errors.Is(err, os.ErrNotExist) || !stampKeyMissingLogged.Swap(true) {
			fmt.Fprintf(p.Out, "%s: review check: %v\n", w.Name, err)
		}
		return chk
	}
	chk.Config = identity.Verify(key, cfg.Stamp, model.KeelConfigStampFields(w.FormID, w.Name, w.Keel.Repo, cfg)...)
	chk.Roles = map[string]model.KeelRoleCheck{}
	verdicts := map[string]json.RawMessage{}
	_ = json.Unmarshal(rawVerdicts, &verdicts)
	d, derr := p.Directory(ctx)
	for _, role := range cfg.Reviewers {
		chk.Roles[role] = checkVerdict(key, w, cfg, role, verdicts[role], d, derr)
	}
	return chk
}

// checkVerdict says whether one reviewer's verdict counts now.
func checkVerdict(key []byte, w *model.Critic, cfg model.KeelReviewConfig, role string, raw json.RawMessage,
	d identity.Directory, derr error) model.KeelRoleCheck {
	if len(raw) == 0 {
		return model.KeelRoleCheck{Why: "no verdict"}
	}
	var v model.KeelVerdict
	if json.Unmarshal(raw, &v) != nil {
		return model.KeelRoleCheck{Why: "unreadable verdict"}
	}
	if !identity.Verify(key, v.Stamp, model.KeelVerdictStampFields(w.FormID, w.Name, role, v)...) {
		return model.KeelRoleCheck{Why: "unstamped or edited"}
	}
	if v.Repo != w.Keel.Repo || v.Doc != cfg.Doc {
		return model.KeelRoleCheck{Why: "stamped for another repo or doc"}
	}
	if derr != nil {
		return model.KeelRoleCheck{Why: "role check: " + derr.Error()}
	}
	form, why := d.Resolve(role)
	switch {
	case why != "":
		return model.KeelRoleCheck{Why: why}
	case v.RoleForm != form:
		return model.KeelRoleCheck{Why: "stamped for role form " + v.RoleForm + ", the role is now " + form}
	case v.ByAria != d.Holder[form]:
		return model.KeelRoleCheck{Why: "recorded by " + v.ByAria + ", the role is now held by " + d.Holder[form] + ": re-verdict"}
	}
	return model.KeelRoleCheck{Valid: true}
}
