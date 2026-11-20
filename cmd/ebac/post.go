package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/gluck/ebac/internal/figaro"
	"github.com/gluck/ebac/internal/form"
	"github.com/gluck/ebac/internal/poll"
	"github.com/gluck/ebac/sdk/model"
)

// cmdPost writes a post onto the critic's form. It never talks to the forge:
// the poll round sends it and writes the receipt back.
func cmdPost(ctx context.Context, g globals, args []string) error {
	fs := flag.NewFlagSet("post", flag.ExitOnError)
	name := fs.String("critic", "", "critic name")
	pr := fs.String("pr", "", "owner/repo#number")
	replyTo := fs.String("reply-to", "", "review thread id to reply in")
	path := fs.String("path", "", "file, for a new inline comment")
	line := fs.Int("line", 0, "line, for a new inline comment")
	event := fs.String("event", "", "COMMENT or APPROVE: submit as a review")
	resolve := fs.Bool("resolve", false, "with --reply-to: resolve the thread (after the reply, if any)")
	react := fs.String("react", "", "with --reply-to: react to the comment that opened the thread (+1 -1 laugh hooray confused heart rocket eyes)")
	bodyFile := fs.String("body-file", "", "file holding the body (- for stdin); optional with --resolve")
	id := fs.String("id", "", "post id (default: generated)")
	rev := fs.String("rev", "", "keel: with --path/--line, anchor the new thread on the diff view at this commit")
	byAria := fs.String("by-aria", "", "keel: must equal the caller's own aria when given (the caller is the writer)")
	byRole := fs.String("by-role", "", "keel: the role the caller writes for; it must hold it (keel stamps aria and role on the comment)")
	_ = fs.Parse(args)

	var body []byte
	var err error
	switch *bodyFile {
	case "":
		if !*resolve && *react == "" {
			return fmt.Errorf("--body-file is required (or --resolve / --react, to act without a reply)")
		}
	case "-":
		body, err = io.ReadAll(os.Stdin)
	default:
		body, err = os.ReadFile(*bodyFile)
	}
	if err != nil {
		return err
	}
	if *id == "" {
		*id = poll.NewPostID(time.Now())
	}
	if err := poll.ValidPostID(*id); err != nil {
		return err
	}
	st, err := form.NewStore(figaro.NewFigaro(g.figaro))
	if err != nil {
		return err
	}
	w, err := st.LoadCritic(ctx, *name)
	if err != nil {
		return err
	}
	snap, err := st.LoadSnapshot(ctx, *name)
	if err != nil {
		return err
	}
	post := &model.Post{PR: *pr, Body: string(body), ReplyTo: *replyTo, Path: *path, Line: *line, Event: strings.ToUpper(*event), Resolve: *resolve, React: *react,
		Rev: *rev, ByAria: *byAria, ByRole: *byRole}
	if w.Mode == model.ModeKeel {
		if err := keelWriter(ctx, g, st, post); err != nil {
			return err
		}
	}
	if why := poll.Authorize(w, snap, "", post); why != "" {
		return fmt.Errorf("refused: %s", why)
	}
	key := "post." + *id
	if f, err := st.ReadForm(ctx, w.FormID); err != nil {
		return err
	} else {
		var posts map[string]json.RawMessage
		_ = json.Unmarshal(f.Keys["post"], &posts)
		if _, taken := posts[*id]; taken {
			return fmt.Errorf("%s already exists on %s: a post is written once", key, w.FormID)
		}
	}
	err = st.Commit(ctx, w.FormID, func(cur map[string]json.RawMessage, _ form.EbacState) (map[string]json.RawMessage, []string) {
		posts := model.ObjectOf(cur["post"])
		posts[*id] = model.MustJSON(post)
		return map[string]json.RawMessage{"post": model.MustJSON(posts)}, nil
	})
	if err != nil {
		return err
	}
	fmt.Printf("%s: queued %s on %s; the next round sends it (receipt: %s.sent)\n", w.Name, *id, w.FormID, key)
	return nil
}

// keelWriter makes the caller the post's writer: by_aria is the caller (an
// explicit --by-aria must match it), the caller must hold by_role, and the
// role form it resolved to is recorded for the flush-time re-check.
func keelWriter(ctx context.Context, g globals, st *form.Store, post *model.Post) error {
	if g.caller == "" {
		return fmt.Errorf("refused: no caller aria (run ebac from a figaro seat: the wrapper passes FIGARO_ARIA as --caller)")
	}
	if post.ByAria != "" && post.ByAria != g.caller {
		return fmt.Errorf("refused: --by-aria %s is not the caller %s", post.ByAria, g.caller)
	}
	post.ByAria = g.caller
	p := &poll.Poller{Store: st, Fig: st.Fig, Out: io.Discard}
	form, why, err := p.PostCheck(ctx, g.caller, post.ByRole)
	if err != nil {
		return fmt.Errorf("role check: %w", err)
	}
	if why != "" {
		return fmt.Errorf("refused: %s", why)
	}
	post.ByRoleForm = form
	return nil
}
