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
)

// teamsChat translates one Teams chat, read through tomb, into a critic's
// inbox. It has its own role form, timer and process, so a slow or dead tomb
// never delays a PR round.
type teamsChat struct{}

func (teamsChat) Name() string { return "teams-chat" }

func (teamsChat) Round(ctx context.Context, p *Poller, w *Critic) error { return p.pollTeams(ctx, w) }

const (
	ModeTeams      Mode = "teams"
	teamsLimit          = 50
	teamsSeenMax        = 300
	teamsBodyBytes      = 1500
	teamsGoneAfter      = 3 // consecutive "gone" answers before the chat is declared gone
	teamsTombWait       = 60 * time.Second
)

type ChatRef struct {
	ID    string `json:"id"`
	Topic string `json:"topic,omitempty"`
	// Quiet senders go to the inbox but never wake the seat.
	Quiet []string `json:"quiet,omitempty"`
}

// TeamsState lives under system.ebac.runtime: the seat never sees it.
type TeamsState struct {
	Seen       map[string]string `json:"seen,omitempty"` // message id -> content digest
	GoneStreak int               `json:"gone_streak,omitempty"`
	GoneSince  string            `json:"gone_since,omitempty"`
}

type TeamsMsg struct {
	ID          string         `json:"id"`
	Version     string         `json:"version"`
	ComposeTime string         `json:"composetime"`
	Arrival     string         `json:"originalarrivaltime"`
	MessageType string         `json:"messagetype"`
	From        string         `json:"imdisplayname"`
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

func (m TeamsMsg) hasImage() bool { return strings.Contains(m.Content, "<img") }

// Tomb runs the tomb CLI. Its cwd is a scratch dir: tomb writes teams.log
// into the cwd unless told otherwise, and --log ” is the otherwise.
type Tomb struct {
	Bin     string
	Timeout time.Duration
}

var errTombGone = errors.New("chat gone")

func (t *Tomb) Messages(ctx context.Context, conv string, limit int) ([]TeamsMsg, error) {
	ctx, cancel := context.WithTimeout(ctx, t.Timeout)
	defer cancel()
	cmd := execCommandContext(ctx, t.Bin, "--log", "", "--json", "--raw", "messages", conv, "--limit", fmt.Sprint(limit))
	cmd.Dir = os.TempDir()
	cmd.Env = append(os.Environ(), "NO_COLOR=1")
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

// A 404 or 403 from the chat API, or a chat tomb cannot find, means the chat
// may be gone; anything else (daemon down, timeout, 5xx, auth) is transient.
var teamsGoneText = regexp.MustCompile(`(?i)returned (404|403)\b|\bnot found\b|forbidden`)

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
func teamsDiff(st *TeamsState, msgs []TeamsMsg, chat *ChatRef) (events []Event, items map[string]InboxItem) {
	items = map[string]InboxItem{}
	first := st.Seen == nil
	if first {
		st.Seen = map[string]string{}
	}
	quiet := map[string]bool{}
	for _, q := range chat.Quiet {
		quiet[strings.ToLower(q)] = true
	}
	for _, m := range msgs {
		if !m.isMessage() || m.ID == "" {
			continue
		}
		d := bodyDigest(m.Content)
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
		tier := TierWake
		if kind == "teams_deleted" || quiet[strings.ToLower(m.From)] {
			tier = TierRecord
		}
		body, cut := cutBody(teamsText(m.Content), teamsBodyBytes)
		events = append(events, Event{Kind: EventKind(kind), Tier: tier, PR: PRKey(chat.Topic), Comment: m.ID, Author: m.From,
			Detail: firstLine(body)})
		it := InboxItem{Chat: chat.ID, Kind: kind, By: m.From, Time: m.ComposeTime, Body: body, Cut: cut, HasImage: m.hasImage(),
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

func (p *Poller) pollTeams(ctx context.Context, w *Critic) error {
	if w.Chat == nil || w.Chat.ID == "" {
		return fmt.Errorf("%s: teams critic without a chat id", w.Name)
	}
	if w.Runtime.Teams == nil {
		w.Runtime.Teams = &TeamsState{}
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
		if st.GoneStreak >= teamsGoneAfter {
			return p.teamsGone(ctx, w, err)
		}
		_ = p.Store.SaveCritic(ctx, w)
		return fmt.Errorf("%s: %w", w.Name, err)
	}
	st.GoneStreak, st.GoneSince = 0, ""
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

	derr := p.deliverTeams(ctx, w)
	if err := p.Store.SaveCritic(ctx, w); err != nil {
		return err
	}
	return derr
}

func (p *Poller) deliverTeams(ctx context.Context, w *Critic) error {
	pending := &Delta{Events: w.Runtime.PendingEvents}
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

func teamsBrief(w *Critic, events []Event) string {
	counts := map[string]int{}
	var from []string
	seenFrom := map[string]bool{}
	for _, e := range events {
		counts[strings.TrimPrefix(string(e.Kind), "teams_")]++
		if e.Tier == TierWake && !seenFrom[e.Author] {
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

// teamsGone marks the chat gone on the form, wakes the seat once and stops.
func (p *Poller) teamsGone(ctx context.Context, w *Critic, cause error) error {
	st := w.Runtime.Teams
	w.Runtime.Stopped = true
	w.Runtime.StopReason = fmt.Sprintf("chat gone since %s after %d consecutive answers: %v", st.GoneSince, st.GoneStreak, cause)
	action := "polling stopped. Delete this form and the critic (ebac rm --critic " + w.Name + " --purge). " +
		"If the chat moved to a new id, make a new critic with ebac add-teams: never repoint this one."
	if err := p.markGone(ctx, w, "chat", "teams chat "+w.Chat.ID+" ("+w.Chat.Topic+")", st.GoneSince, cause, action); err != nil {
		return err
	}
	p.wakeOnce(ctx, w, fmt.Sprintf("ebac %s: the chat %q is gone (%v). Polling stopped; the form's key gone says what to do.", w.Name, w.Chat.Topic, cause))
	_ = disableTimer(w.Name)
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
	quiet := fs.String("quiet", "", "comma-separated senders that never wake the seat")
	every := fs.Duration("every", 60*time.Second, "poll interval")
	_ = fs.Parse(args)
	if err := ValidName(*name); err != nil {
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
	w := &Critic{Name: *name, Mode: ModeTeams, Source: "teams-chat", CreatedAt: time.Now().UTC(), FigaroSock: g.figaro,
		Chat: &ChatRef{ID: *chat, Topic: *topic}, Stop: StopConditions{}}
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
		if err := ValidAriaID(*aria); err != nil {
			return fmt.Errorf("critic created (role %s), but --aria is bad: %w", formID, err)
		}
		if err := st.Fig.Cast(ctx, *aria, formID); err != nil {
			return fmt.Errorf("critic created (role %s), but casting %s failed: %w", formID, *aria, err)
		}
	}
	if err := enableTeamsTimer(*name, *every); err != nil {
		return fmt.Errorf("critic created (role %s), but its timer: %w", formID, err)
	}
	fmt.Printf("teams critic %q: role %s, chat %s (%s), seat %s, every %s\n", *name, formID, *chat, *topic, *aria, *every)
	return nil
}

// enableTeamsTimer replaces the template's 5-minute trigger and jitter: a drop-in
// adds to a list unless it first resets it.
func enableTeamsTimer(name string, every time.Duration) error {
	dir, err := unitDir()
	if err != nil {
		return err
	}
	dd := fmt.Sprintf("%s/ebac@%s.timer.d", dir, name)
	if err := os.MkdirAll(dd, 0o755); err != nil {
		return err
	}
	body := fmt.Sprintf("[Timer]\nOnUnitActiveSec=\nOnUnitActiveSec=%ds\nOnBootSec=60s\nRandomizedDelaySec=0\nAccuracySec=5s\n", int(every.Seconds()))
	if err := os.WriteFile(dd+"/interval.conf", []byte(body), 0o644); err != nil {
		return err
	}
	if err := systemctl("daemon-reload"); err != nil {
		return err
	}
	return systemctl("enable", "--now", fmt.Sprintf("ebac@%s.timer", name))
}
