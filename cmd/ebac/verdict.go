package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"time"

	"github.com/gluck/ebac/internal/figaro"
	"github.com/gluck/ebac/internal/form"
	"github.com/gluck/ebac/internal/identity"
	"github.com/gluck/ebac/internal/poll"
	keelcfg "github.com/gluck/ebac/sdk/cfg/keel"
	"github.com/gluck/ebac/sdk/model"
)

// cmdVerdict records the caller's verdict on a keel critic's review: the
// caller must hold the role, the doc's revision must be known, and the
// verdict is stamped so the critic counts it (a verdict set by hand never
// counts).
func cmdVerdict(ctx context.Context, g globals, args []string) error {
	fs := flag.NewFlagSet("verdict", flag.ExitOnError)
	name := fs.String("critic", "", "keel critic with a review key")
	role := fs.String("role", "", "the reviewer role the caller holds")
	verdict := fs.String("verdict", "", "pass | changes | comment")
	note := fs.String("note", "", "optional one-line note")
	rev := fs.String("rev", "", "the doc revision you reviewed (must be the doc's current revision on keel)")
	_ = fs.Parse(args)
	switch *verdict {
	case "pass", "changes", "comment":
	default:
		return fmt.Errorf("--verdict %q: want pass, changes or comment", *verdict)
	}
	if g.caller == "" {
		return fmt.Errorf("refused: no caller aria (run ebac from a figaro seat: the wrapper passes FIGARO_ARIA as --caller)")
	}
	st, err := form.NewStore(figaro.NewFigaro(g.figaro))
	if err != nil {
		return err
	}
	w, err := st.LoadCritic(ctx, *name)
	if err != nil {
		return err
	}
	if w.Mode != model.ModeKeel || w.Keel == nil {
		return fmt.Errorf("refused: %s is not a keel critic", *name)
	}
	f, err := st.ReadForm(ctx, w.FormID)
	if err != nil {
		return err
	}
	var cfg model.KeelReviewConfig
	if err := json.Unmarshal(f.Keys[keelcfg.ReviewKey], &cfg); err != nil || cfg.Doc == "" {
		return fmt.Errorf("refused: %s has no review configured (ebac review)", *name)
	}
	key, err := identity.LoadKey(identity.KeyPath(), true)
	if err != nil {
		return err
	}
	if !identity.Verify(key, cfg.Stamp, model.KeelConfigStampFields(w.FormID, w.Name, w.Keel.Repo, cfg)...) {
		return fmt.Errorf("refused: %s's review key was not written by ebac review (or was edited since)", *name)
	}
	runner, err := newRunner(g)
	if err != nil {
		return err
	}
	p := &poll.Poller{Store: st, Fig: st.Fig, Out: roundOut(io.Discard), Plugins: runner}
	current, err := p.DocRev(ctx, w, cfg.Doc)
	if err != nil {
		return fmt.Errorf("refused: the revision of %s could not be read from keel: %w", cfg.Doc, err)
	}
	d, err := p.Directory(ctx)
	if err != nil {
		return fmt.Errorf("role check: %w", err)
	}
	roleForm, err := verdictGuard(d, g.caller, *role, *rev, current, cfg)
	if err != nil {
		return err
	}
	rs := struct{ Rev string }{current}
	v := model.KeelVerdict{Verdict: *verdict, Rev: rs.Rev, At: time.Now().UTC().Format(time.RFC3339), ByAria: g.caller,
		Note: *note, Repo: w.Keel.Repo, Doc: cfg.Doc, RoleForm: roleForm}
	v.Stamp = identity.Stamp(key, model.KeelVerdictStampFields(w.FormID, w.Name, *role, v)...)
	err = st.Commit(ctx, w.FormID, func(cur map[string]json.RawMessage, _ form.EbacState) (map[string]json.RawMessage, []string) {
		all := map[string]json.RawMessage{}
		_ = json.Unmarshal(cur[keelcfg.VerdictKey], &all)
		all[*role] = model.MustJSON(v)
		return map[string]json.RawMessage{keelcfg.VerdictKey: model.MustJSON(all)}, nil
	})
	if err != nil {
		return err
	}
	fmt.Printf("verdict %s by %s (%s) on %s at %s: recorded; review_state updates on the next round\n", *verdict, *role, g.caller[:8], cfg.Doc, rs.Rev[:min(9, len(rs.Rev))])
	return nil
}

// verdictGuard is every refusal of `ebac verdict` that does not need I/O:
// the caller is required and must hold the reviewer role (holder only), the
// role must be one of the review's reviewers, the reviewed revision must be
// the doc's current one, and the caller must be provably independent of the
// author side.
func verdictGuard(d identity.Directory, caller, role, rev, current string, cfg model.KeelReviewConfig) (roleForm string, err error) {
	if caller == "" {
		return "", fmt.Errorf("refused: no caller aria (run ebac from a figaro seat: the wrapper passes FIGARO_ARIA as --caller)")
	}
	if !contains(cfg.Reviewers, role) {
		return "", fmt.Errorf("refused: %s is not a reviewer of this review (%v)", role, cfg.Reviewers)
	}
	if current == "" {
		return "", fmt.Errorf("refused: keel has no revision of %s yet", cfg.Doc)
	}
	if rev != current {
		return "", fmt.Errorf("refused: --rev %q is not the doc's current revision %s: review that and retry", rev, current)
	}
	roleForm, ok, why := d.MayVerdict(caller, role)
	if !ok {
		return "", fmt.Errorf("refused: %s", why)
	}
	af, why := d.Resolve(cfg.AuthorRole)
	if why != "" {
		return "", fmt.Errorf("refused: author role: %s", why)
	}
	if ok, why := d.Independent(caller, d.Holder[af]); !ok {
		return "", fmt.Errorf("refused: %s", why)
	}
	return roleForm, nil
}

// reviewGuard is every refusal of `ebac review` that does not need I/O: a
// caller, the author role held by it (or a fork of it), every role resolving,
// and the author role not among the reviewers.
func reviewGuard(d identity.Directory, caller, author string, reviewers []string) error {
	if caller == "" {
		return fmt.Errorf("refused: no caller aria (run ebac from a figaro seat: the wrapper passes FIGARO_ARIA as --caller)")
	}
	if len(reviewers) == 0 || author == "" {
		return fmt.Errorf("refused: --reviewers and --author-role are required")
	}
	if _, ok, why := d.MayPost(caller, author); !ok {
		return fmt.Errorf("refused: only the author role's holder (or a fork of it) sets its review: %s", why)
	}
	for _, r := range reviewers {
		if _, why := d.Resolve(r); why != "" {
			return fmt.Errorf("refused: %s", why)
		}
	}
	if contains(reviewers, author) {
		return fmt.Errorf("refused: the author role %s cannot be a reviewer", author)
	}
	return nil
}

func contains(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}

// cmdReview writes a keel critic's review key, stamped: the doc, the named
// reviewer roles (required, non-empty) and the author role whose side may not
// record verdicts. Every role is checked against the roster now.
func cmdReview(ctx context.Context, g globals, args []string) error {
	fs := flag.NewFlagSet("review", flag.ExitOnError)
	name := fs.String("critic", "", "keel critic")
	doc := fs.String("doc", "", "the reviewed path in the critic's repo")
	reviewers := fs.String("reviewers", "", "comma-separated reviewer roles (required)")
	author := fs.String("author-role", "", "the doc's author role: its holder and forks may not record verdicts (required)")
	_ = fs.Parse(args)
	if g.caller == "" {
		return fmt.Errorf("refused: no caller aria (run ebac from a figaro seat: the wrapper passes FIGARO_ARIA as --caller)")
	}
	rs := splitList(*reviewers)
	if *doc == "" || len(rs) == 0 || *author == "" {
		return fmt.Errorf("--doc, --reviewers and --author-role are required")
	}
	st, err := form.NewStore(figaro.NewFigaro(g.figaro))
	if err != nil {
		return err
	}
	w, err := st.LoadCritic(ctx, *name)
	if err != nil {
		return err
	}
	if w.Mode != model.ModeKeel || w.Keel == nil || !model.KeelPathMatches(*doc, w.Keel.Paths) {
		return fmt.Errorf("refused: %s is not a keel critic over %s", *name, *doc)
	}
	p := &poll.Poller{Store: st, Fig: st.Fig, Out: io.Discard}
	d, err := p.Directory(ctx)
	if err != nil {
		return fmt.Errorf("role check: %w", err)
	}
	if err := reviewGuard(d, g.caller, *author, rs); err != nil {
		return err
	}
	key, err := identity.LoadKey(identity.KeyPath(), true)
	if err != nil {
		return err
	}
	cfg := model.KeelReviewConfig{Doc: *doc, Reviewers: rs, AuthorRole: *author, ByAria: g.caller, At: time.Now().UTC().Format(time.RFC3339)}
	cfg.Stamp = identity.Stamp(key, model.KeelConfigStampFields(w.FormID, w.Name, w.Keel.Repo, cfg)...)
	if err := st.Commit(ctx, w.FormID, func(map[string]json.RawMessage, form.EbacState) (map[string]json.RawMessage, []string) {
		return map[string]json.RawMessage{keelcfg.ReviewKey: model.MustJSON(cfg)}, nil
	}); err != nil {
		return err
	}
	fmt.Printf("review of %s on %s: reviewers %v, author %s (stamped)\n", *doc, *name, rs, *author)
	return nil
}
