package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/gluck/ebac/internal/forge"
	"github.com/gluck/ebac/internal/model"
	"github.com/gluck/ebac/internal/systemd"
)

// teamsChat translates one Teams chat, read through tomb, into a critic's
// inbox. It has its own role form, timer and process, so a slow or dead tomb
// never delays a PR round.
type teamsChat struct{}

func (teamsChat) Name() string { return "teams-chat" }

func (teamsChat) Round(ctx context.Context, p *Poller, w *model.Critic) error {
	return pollTeams(ctx, p, w)
}

const (
	teamsLimit     = 50
	teamsSeenMax   = 300
	teamsNamesMax  = 200
	teamsBodyBytes = 1500
	teamsGoneAfter = 3 // consecutive "gone" answers before the chat is declared gone
)

const teamsTombWait = 60 * time.Second

// teamsBlindAfter is how long the watcher may fail to read the chat before it
// tells the seat it is not hearing it. A var so a test can shorten it.
var teamsBlindAfter = 10 * time.Minute

type TeamsMsg struct {
	ID          string         `json:"id"`
	Version     string         `json:"version"`
	ComposeTime string         `json:"composetime"`
	Arrival     string         `json:"originalarrivaltime"`
	MessageType string         `json:"messagetype"`
	From        string         `json:"imdisplayname"`
	FromURL     string         `json:"from"`
	TokenName   string         `json:"fromDisplayNameInToken"`
	Content     string         `json:"content"`
	Properties  map[string]any `json:"properties"`
}

func (m TeamsMsg) isMessage() bool {
	return m.MessageType == "RichText/Html" || m.MessageType == "Text" || strings.HasPrefix(m.MessageType, "RichText")
}

func (m TeamsMsg) deleted() bool {
	if m.Properties != nil {
		if v, ok := m.Properties["deletetime"]; ok && v != nil && fmt.Sprint(v) != "" && fmt.Sprint(v) != "0" {
			return true
		}
	}
	return strings.TrimSpace(m.Content) == ""
}

// senderID is the last segment of the from URL: 8:orgid:<guid> for a person.
func (m TeamsMsg) senderID() string {
	return m.FromURL[strings.LastIndex(m.FromURL, "/")+1:]
}

// sender names who wrote m: the row's name, the token's, a name learned from
// another row of the same sender, and at worst the sender id itself.
func (m TeamsMsg) sender(names map[string]string) string {
	for _, n := range []string{m.From, m.TokenName, names[m.senderID()], m.senderID()} {
		if n = strings.TrimSpace(n); n != "" {
			return n
		}
	}
	return "unknown sender"
}

func (m TeamsMsg) hasImage() bool { return strings.Contains(m.Content, "<img") }

// Tomb runs the tomb CLI. Its cwd is a scratch dir: tomb writes teams.log
// into the cwd unless told otherwise, and --log ” is the otherwise.
type Tomb struct {
	Bin     string
	Timeout time.Duration
	// Cred mints the Teams token (band-cred teams-ebac), handed to tomb on fd 3
	// with TOMB_AAD_TOKEN_FD=3: the env carries the fd number, never the token.
	// nil only in tests that fake tomb.
	Cred *forge.Cred
}

var errTombGone = errors.New("chat gone")

func (t *Tomb) Messages(ctx context.Context, conv string, limit int) ([]TeamsMsg, error) {
	ctx, cancel := context.WithTimeout(ctx, t.Timeout)
	defer cancel()
	cmd := forge.ExecCommandContext(ctx, t.Bin, "--log", "", "--json", "--raw", "messages", conv, "--limit", fmt.Sprint(limit))
	cmd.Dir = os.TempDir()
	cmd.Env = append(os.Environ(), "NO_COLOR=1")
	if t.Cred != nil {
		tok, err := t.Cred.Token(ctx)
		if err != nil {
			return nil, err
		}
		r, w, err := os.Pipe()
		if err != nil {
			return nil, err
		}
		defer r.Close()
		// A token is a few KB, far under the pipe buffer: write it all, then close.
		if _, err := w.WriteString(tok); err != nil {
			w.Close()
			return nil, err
		}
		w.Close()
		cmd.ExtraFiles = []*os.File{r} // fd 3 in the child
		cmd.Env = append(cmd.Env, "TOMB_AAD_TOKEN_FD=3")
	}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if ctx.Err() == context.DeadlineExceeded {
			return nil, fmt.Errorf("tomb timed out after %s", t.Timeout)
		}
		if teamsGoneText.MatchString(msg) {
			return nil, fmt.Errorf("%w: %s", errTombGone, msg)
		}
		if msg == "" {
			msg = err.Error()
		}
		return nil, fmt.Errorf("tomb: %s", msg)
	}
	var resp struct {
		Messages []TeamsMsg `json:"messages"`
	}
	if err := json.Unmarshal(out.Bytes(), &resp); err != nil {
		return nil, fmt.Errorf("tomb: unreadable answer: %w", err)
	}
	return resp.Messages, nil
}

// Only the CHAT API's 404 or 403 means the chat may be gone. tomb's auth
// errors read "auth service returned 403: ..." and an expired login must stay
// transient: matching any "returned 403" would stop the critic for good after
// three rounds of a sign-in problem.
var teamsGoneText = regexp.MustCompile(`\bchat API returned (404|403)\b`)

var (
	htmlBreak = regexp.MustCompile(`(?i)<br\s*/?>|</p>|</div>|</li>`)
	htmlTag   = regexp.MustCompile(`<[^>]*>`)
	blankRuns = regexp.MustCompile(`\n{3,}`)
)

func teamsText(content string) string {
	s := htmlBreak.ReplaceAllString(content, "\n")
	s = htmlTag.ReplaceAllString(s, "")
	s = html.UnescapeString(s)
	s = blankRuns.ReplaceAllString(strings.ReplaceAll(s, "\r", ""), "\n\n")
	return strings.TrimSpace(s)
}

// teamsDiff compares a fetch with what was seen. The first round only
// records: a watcher that starts by announcing fifty old messages is noise.
func teamsDiff(st *model.TeamsState, msgs []TeamsMsg, chat *model.ChatRef) (events []model.Event, items map[string]InboxItem) {
	items = map[string]InboxItem{}
	first := st.Seen == nil
	if first {
		st.Seen = map[string]string{}
	}
	if st.Names == nil {
		st.Names = map[string]string{}
	}
	for _, m := range msgs {
		if n := strings.TrimSpace(m.From); n != "" && m.isMessage() && m.senderID() != "" {
			if _, ok := st.Names[m.senderID()]; ok || len(st.Names) < teamsNamesMax {
				st.Names[m.senderID()] = n
			}
		}
	}
	quiet := map[string]bool{}
	for _, q := range chat.Quiet {
		quiet[strings.ToLower(q)] = true
	}
	// A quiet sender is matched by name, by sender id, or by the bare orgid guid.
	isQuiet := func(m TeamsMsg, who string) bool {
		id := strings.ToLower(m.senderID())
		return quiet[strings.ToLower(who)] || (id != "" && (quiet[id] || quiet[strings.TrimPrefix(id, "8:orgid:")]))
	}
	for _, m := range msgs {
		if !m.isMessage() || m.ID == "" {
			continue
		}
		d := model.BodyDigest(m.Content)
		if m.deleted() {
			d = "deleted"
		}
		prev, seen := st.Seen[m.ID]
		st.Seen[m.ID] = d
		if first || prev == d {
			continue
		}
		kind := "teams_new"
		switch {
		case seen && d == "deleted":
			kind = "teams_deleted"
		case seen:
			kind = "teams_edited"
		case d == "deleted":
			continue // created and deleted between two rounds
		}
		tier := model.TierWake
		who := m.sender(st.Names)
		if kind == "teams_deleted" || isQuiet(m, who) {
			tier = model.TierRecord
		}
		body, cut := model.CutBody(teamsText(m.Content), teamsBodyBytes)
		events = append(events, model.Event{Kind: model.EventKind(kind), Tier: tier, PR: model.PRKey(chat.Topic), Comment: m.ID, Author: who,
			Detail: model.FirstLine(body)})
		it := InboxItem{Chat: chat.ID, Kind: kind, By: who, FromID: m.senderID(), Time: m.ComposeTime, Body: body, Cut: cut, HasImage: m.hasImage(),
			Fetch: fmt.Sprintf("tomb --log '' --json --raw messages '%s' --limit %d | jq '.messages[] | select(.id==\"%s\")'", chat.ID, teamsLimit, m.ID)}
		if it.HasImage {
			it.Fetch += fmt.Sprintf("; images: tomb --log '' --download-images <dir> messages '%s' --limit %d", chat.ID, teamsLimit)
		}
		items["teams-"+m.ID] = it
	}
	if len(st.Seen) > teamsSeenMax {
		ids := make([]string, 0, len(st.Seen))
		for id := range st.Seen {
			ids = append(ids, id)
		}
		// Teams message ids are millisecond timestamps: oldest sort first.
		sort.Slice(ids, func(i, j int) bool {
			if len(ids[i]) != len(ids[j]) {
				return len(ids[i]) < len(ids[j])
			}
			return ids[i] < ids[j]
		})
		for _, id := range ids[:len(ids)-teamsSeenMax] {
			delete(st.Seen, id)
		}
	}
	return events, items
}

func pollTeams(ctx context.Context, p *Poller, w *model.Critic) error {
	if w.Chat == nil || w.Chat.ID == "" {
		return fmt.Errorf("%s: teams critic without a chat id", w.Name)
	}
	if w.Runtime.Teams == nil {
		w.Runtime.Teams = &model.TeamsState{}
	}
	st := w.Runtime.Teams
	w.Runtime.Round++
	w.Runtime.LastPolled = time.Now().UTC()

	msgs, err := p.Tomb.Messages(ctx, w.Chat.ID, teamsLimit)
	if err != nil {
		if errors.Is(err, errTombGone) {
			st.GoneStreak++
			if st.GoneSince == "" {
				st.GoneSince = time.Now().UTC().Format(time.RFC3339)
			}
		} else {
			st.GoneStreak = 0
		}
		w.Runtime.ConsecutiveFailures++
		w.Runtime.LastError = err.Error()
		if st.FailSince == "" {
			st.FailSince = time.Now().UTC().Format(time.RFC3339)
		}
		if st.GoneStreak >= teamsGoneAfter {
			return teamsGone(ctx, p, w, err)
		}
		teamsBlind(ctx, p, w, err)
		_ = p.Store.SaveCritic(ctx, w)
		return fmt.Errorf("%s: %w", w.Name, err)
	}
	st.GoneStreak, st.GoneSince = 0, ""
	if st.Blind {
		if err := p.Store.commit(ctx, w.FormID, func(map[string]json.RawMessage, ebacState) (map[string]json.RawMessage, []string) {
			return nil, []string{"blind"}
		}); err != nil {
			return fmt.Errorf("%s: clear blind marker: %w", w.Name, err)
		}
		fmt.Fprintf(p.Out, "%s: hearing the chat again (blind since %s)\n", w.Name, st.FailSince)
	}
	st.FailSince, st.Blind, st.BlindErr = "", false, ""
	w.Runtime.ConsecutiveFailures = 0
	w.Runtime.LastError = ""
	w.Runtime.LastSuccess = time.Now().UTC()

	events, items := teamsDiff(st, msgs, w.Chat)
	now := time.Now().UTC().Format(time.RFC3339)
	for k, it := range items {
		it.At = now
		items[k] = it
	}
	if len(events) > 0 {
		if len(w.Runtime.PendingEvents) == 0 {
			w.Runtime.PendingSince = time.Now().UTC()
		}
		w.Runtime.PendingEvents = mergeEvents(w.Runtime.PendingEvents, events)
	}
	if err := p.Store.SaveRound(ctx, w, nil, nil, items); err != nil {
		return err
	}
	renew(ctx, p.Fig, w.FormID)

	derr := deliverTeams(ctx, p, w)
	if err := p.Store.SaveCritic(ctx, w); err != nil {
		return err
	}
	return derr
}

func deliverTeams(ctx context.Context, p *Poller, w *model.Critic) error {
	pending := &model.Delta{Events: w.Runtime.PendingEvents}
	if !pending.WakeWorthy() {
		if len(w.Runtime.PendingEvents) > 0 {
			fmt.Fprintf(p.Out, "%s: round %d quiet (%d recorded)\n", w.Name, w.Runtime.Round, len(w.Runtime.PendingEvents))
		} else {
			fmt.Fprintf(p.Out, "%s: round %d quiet\n", w.Name, w.Runtime.Round)
		}
		w.Runtime.PendingEvents, w.Runtime.PendingSince = nil, time.Time{}
		return nil
	}
	if !p.roleHasHolder(ctx, w.FormID) {
		w.Runtime.LastError = "role has no reachable target-aria; cast a figaro into " + w.FormID
		fmt.Fprintf(p.Out, "%s: %s\n", w.Name, w.Runtime.LastError)
		return nil
	}
	if err := p.Fig.Send(ctx, w.FormID, teamsBrief(w, w.Runtime.PendingEvents)); err != nil {
		w.Runtime.RetryCount++
		w.Runtime.LastError = fmt.Sprintf("delivery attempt %d failed: %v", w.Runtime.RetryCount, err)
		return fmt.Errorf("%s: %s", w.Name, w.Runtime.LastError)
	}
	fmt.Fprintf(p.Out, "%s: alerted %s — %d message event(s)\n", w.Name, w.FormID, len(w.Runtime.PendingEvents))
	w.Runtime.PendingEvents, w.Runtime.PendingSince = nil, time.Time{}
	w.Runtime.RetryCount = 0
	w.Runtime.LastDelivered = time.Now().UTC()
	w.Runtime.DeliveredRound = w.Runtime.Round
	return nil
}

func teamsBrief(w *model.Critic, events []model.Event) string {
	counts := map[string]int{}
	var from []string
	seenFrom := map[string]bool{}
	for _, e := range events {
		counts[strings.TrimPrefix(string(e.Kind), "teams_")]++
		if e.Tier == model.TierWake && !seenFrom[e.Author] {
			seenFrom[e.Author] = true
			from = append(from, e.Author)
		}
	}
	var parts []string
	for _, k := range []string{"new", "edited", "deleted"} {
		if counts[k] > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", counts[k], k))
		}
	}
	return fmt.Sprintf("ebac %s round %d: %s in %q (from %s). The messages are in your studied form %s, key inbox "+
		"(text cut at %d B; each entry has the exact tomb command for the rest and any image). "+
		"Not studying %s is a bug: `figaro study %s`. When finished: ebac ack --critic %s --round %d\n",
		w.Name, w.Runtime.Round, strings.Join(parts, ", "), w.Chat.Topic, strings.Join(from, ", "),
		w.FormID, teamsBodyBytes, w.FormID, w.FormID, w.Name, w.Runtime.Round)
}

// teamsBlind tells the seat, once, that the chat has been unreadable for
// longer than teamsBlindAfter: the form's key blind says since when and the
// last error, refreshed only when the error changes, and is removed by the
// first good round.
func teamsBlind(ctx context.Context, p *Poller, w *model.Critic, cause error) {
	st := w.Runtime.Teams
	since, err := time.Parse(time.RFC3339, st.FailSince)
	if err != nil || time.Since(since) < teamsBlindAfter {
		return
	}
	if st.Blind && st.BlindErr == cause.Error() {
		return
	}
	marker := map[string]string{"since": st.FailSince, "last_error": cause.Error(), "updated": time.Now().UTC().Format(time.RFC3339),
		"meaning": "ebac cannot read the chat " + w.Chat.Topic + ": messages since then are unseen until this key disappears"}
	if err := p.Store.commit(ctx, w.FormID, func(map[string]json.RawMessage, ebacState) (map[string]json.RawMessage, []string) {
		return map[string]json.RawMessage{"blind": model.MustJSON(marker)}, nil
	}); err != nil {
		fmt.Fprintf(p.Out, "%s: could not write blind marker: %v\n", w.Name, err)
		return
	}
	if !st.Blind {
		p.wakeOnce(ctx, w, fmt.Sprintf("ebac %s: BLIND since %s: cannot read %q (%v). Messages are unseen until your form's key blind disappears; it clears itself on recovery. If it persists, the Boss may need to sign tomb in again.",
			w.Name, st.FailSince, w.Chat.Topic, cause))
		fmt.Fprintf(p.Out, "%s: blind since %s: %v\n", w.Name, st.FailSince, cause)
	}
	st.Blind, st.BlindErr = true, cause.Error()
}

// teamsGone marks the chat gone on the form, wakes the seat once and stops.
func teamsGone(ctx context.Context, p *Poller, w *model.Critic, cause error) error {
	st := w.Runtime.Teams
	w.Runtime.Stopped = true
	w.Runtime.StopReason = fmt.Sprintf("chat gone since %s after %d consecutive answers: %v", st.GoneSince, st.GoneStreak, cause)
	action := "polling stopped. Delete this form and the critic (ebac rm --critic " + w.Name + " --purge). " +
		"If the chat moved to a new id, make a new critic with ebac add-teams: never repoint this one."
	if err := p.markGone(ctx, w, "chat", "teams chat "+w.Chat.ID+" ("+w.Chat.Topic+")", st.GoneSince, cause, action); err != nil {
		return err
	}
	p.wakeOnce(ctx, w, fmt.Sprintf("ebac %s: the chat %q is gone (%v). Polling stopped; the form's key gone says what to do.", w.Name, w.Chat.Topic, cause))
	_ = systemd.DisableTimer(w.Name)
	fmt.Fprintf(p.Out, "%s: %s\n", w.Name, w.Runtime.StopReason)
	return p.Store.SaveCritic(ctx, w)
}

// cmdAddTeams mints a teams critic: role form, critic, seat, timer.
func cmdAddTeams(ctx context.Context, g globals, args []string) error {
	fs := newFlagSet("add-teams")
	name := fs.String("name", "", "critic name, e.g. teams-md1-cowork")
	chat := fs.String("chat", "", "Teams conversation id (19:...@thread.v2)")
	topic := fs.String("topic", "", "chat topic, used to find it again")
	aria := fs.String("aria", "", "seat: the aria cast into the role")
	quiet := fs.String("quiet", "", "comma-separated senders that never wake the seat: display names or sender ids (8:orgid:<guid> or the bare guid)")
	every := fs.Duration("every", 60*time.Second, "poll interval")
	_ = fs.Parse(args)
	if err := model.ValidName(*name); err != nil {
		return err
	}
	if *chat == "" {
		return fmt.Errorf("--chat is required")
	}
	st, err := NewStore(NewFigaro(g.figaro))
	if err != nil {
		return err
	}
	if _, err := st.LoadCritic(ctx, *name); err == nil {
		return fmt.Errorf("critic %q already exists", *name)
	}
	w := &model.Critic{Name: *name, Mode: model.ModeTeams, Source: "teams-chat", CreatedAt: time.Now().UTC(), FigaroSock: g.figaro, EverySec: int(every.Seconds()),
		Chat: &model.ChatRef{ID: *chat, Topic: *topic}, Stop: model.StopConditions{}}
	for _, q := range strings.Split(*quiet, ",") {
		if q = strings.TrimSpace(q); q != "" {
			w.Chat.Quiet = append(w.Chat.Quiet, q)
		}
	}
	formID, err := st.Fig.FormNew(ctx, "ebac-"+*name)
	if err != nil {
		return fmt.Errorf("mint role form: %w", err)
	}
	w.FormID = formID
	if err := st.SaveCritic(ctx, w); err != nil {
		return err
	}
	if *aria != "" {
		if err := model.ValidAriaID(*aria); err != nil {
			return fmt.Errorf("critic created (role %s), but --aria is bad: %w", formID, err)
		}
		if err := st.Fig.Cast(ctx, *aria, formID); err != nil {
			return fmt.Errorf("critic created (role %s), but casting %s failed: %w", formID, *aria, err)
		}
	}
	if err := systemd.EnableTimer(*name, *every); err != nil {
		return fmt.Errorf("critic created (role %s), but its timer: %w", formID, err)
	}
	fmt.Printf("teams critic %q: role %s, chat %s (%s), seat %s, every %s\n", *name, formID, *chat, *topic, *aria, *every)
	return nil
}
