package teams

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/gluck/ebac/internal/figaro"
	"github.com/gluck/ebac/internal/form"
	"github.com/gluck/ebac/internal/model"
	"github.com/gluck/ebac/internal/poll"
	"github.com/gluck/ebac/internal/systemd"
	"github.com/gluck/ebac/internal/tomb"
)

// teamsChat translates one Teams chat, read through tomb, into a critic's
// inbox. It has its own role form, timer and process, so a slow or dead tomb
// never delays a PR round.
type Source struct{}

func (Source) Name() string { return "teams-chat" }

func (Source) Round(ctx context.Context, p *poll.Poller, w *model.Critic) error {
	return pollTeams(ctx, p, w)
}

const (
	teamsLimit     = 50
	teamsSeenMax   = 300
	teamsNamesMax  = 200
	teamsBodyBytes = 1500
	teamsGoneAfter = 3 // consecutive "gone" answers before the chat is declared gone
)

// teamsBlindAfter is how long the watcher may fail to read the chat before it
// tells the seat it is not hearing it. A var so a test can shorten it.
var teamsBlindAfter = 10 * time.Minute

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
func teamsDiff(st *model.TeamsState, msgs []tomb.TeamsMsg, chat *model.ChatRef) (events []model.Event, items map[string]form.InboxItem) {
	items = map[string]form.InboxItem{}
	first := st.Seen == nil
	if first {
		st.Seen = map[string]string{}
	}
	if st.Names == nil {
		st.Names = map[string]string{}
	}
	for _, m := range msgs {
		if n := strings.TrimSpace(m.From); n != "" && m.IsMessage() && m.SenderID() != "" {
			if _, ok := st.Names[m.SenderID()]; ok || len(st.Names) < teamsNamesMax {
				st.Names[m.SenderID()] = n
			}
		}
	}
	quiet := map[string]bool{}
	for _, q := range chat.Quiet {
		quiet[strings.ToLower(q)] = true
	}
	// A quiet sender is matched by name, by sender id, or by the bare orgid guid.
	isQuiet := func(m tomb.TeamsMsg, who string) bool {
		id := strings.ToLower(m.SenderID())
		return quiet[strings.ToLower(who)] || (id != "" && (quiet[id] || quiet[strings.TrimPrefix(id, "8:orgid:")]))
	}
	for _, m := range msgs {
		if !m.IsMessage() || m.ID == "" {
			continue
		}
		d := model.BodyDigest(m.Content)
		if m.Deleted() {
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
		who := m.Sender(st.Names)
		if kind == "teams_deleted" || isQuiet(m, who) {
			tier = model.TierRecord
		}
		body, cut := model.CutBody(teamsText(m.Content), teamsBodyBytes)
		events = append(events, model.Event{Kind: model.EventKind(kind), Tier: tier, PR: model.PRKey(chat.Topic), Comment: m.ID, Author: who,
			Detail: model.FirstLine(body)})
		it := form.InboxItem{Chat: chat.ID, Kind: kind, By: who, FromID: m.SenderID(), Time: m.ComposeTime, Body: body, Cut: cut, HasImage: m.HasImage(),
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

func pollTeams(ctx context.Context, p *poll.Poller, w *model.Critic) error {
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
		if errors.Is(err, tomb.ErrTombGone) {
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
		if err := p.Store.Commit(ctx, w.FormID, func(map[string]json.RawMessage, form.EbacState) (map[string]json.RawMessage, []string) {
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
		w.Runtime.PendingEvents = poll.MergeEvents(w.Runtime.PendingEvents, events)
	}
	if err := p.Store.SaveRound(ctx, w, nil, nil, items); err != nil {
		return err
	}
	figaro.Renew(ctx, p.Fig, w.FormID)

	derr := deliverTeams(ctx, p, w)
	if err := p.Store.SaveCritic(ctx, w); err != nil {
		return err
	}
	return derr
}

func deliverTeams(ctx context.Context, p *poll.Poller, w *model.Critic) error {
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
	if !p.RoleHasHolder(ctx, w.FormID) {
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
func teamsBlind(ctx context.Context, p *poll.Poller, w *model.Critic, cause error) {
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
	if err := p.Store.Commit(ctx, w.FormID, func(map[string]json.RawMessage, form.EbacState) (map[string]json.RawMessage, []string) {
		return map[string]json.RawMessage{"blind": model.MustJSON(marker)}, nil
	}); err != nil {
		fmt.Fprintf(p.Out, "%s: could not write blind marker: %v\n", w.Name, err)
		return
	}
	if !st.Blind {
		p.WakeOnce(ctx, w, fmt.Sprintf("ebac %s: BLIND since %s: cannot read %q (%v). Messages are unseen until your form's key blind disappears; it clears itself on recovery. If it persists, the Boss may need to sign tomb in again.",
			w.Name, st.FailSince, w.Chat.Topic, cause))
		fmt.Fprintf(p.Out, "%s: blind since %s: %v\n", w.Name, st.FailSince, cause)
	}
	st.Blind, st.BlindErr = true, cause.Error()
}

// teamsGone marks the chat gone on the form, wakes the seat once and stops.
func teamsGone(ctx context.Context, p *poll.Poller, w *model.Critic, cause error) error {
	st := w.Runtime.Teams
	w.Runtime.Stopped = true
	w.Runtime.StopReason = fmt.Sprintf("chat gone since %s after %d consecutive answers: %v", st.GoneSince, st.GoneStreak, cause)
	action := "polling stopped. Delete this form and the critic (ebac rm --critic " + w.Name + " --purge). " +
		"If the chat moved to a new id, make a new critic with ebac add-teams: never repoint this one."
	if err := p.MarkGone(ctx, w, "chat", "teams chat "+w.Chat.ID+" ("+w.Chat.Topic+")", st.GoneSince, cause, action); err != nil {
		return err
	}
	p.WakeOnce(ctx, w, fmt.Sprintf("ebac %s: the chat %q is gone (%v). Polling stopped; the form's key gone says what to do.", w.Name, w.Chat.Topic, cause))
	_ = systemd.DisableTimer(w.Name)
	fmt.Fprintf(p.Out, "%s: %s\n", w.Name, w.Runtime.StopReason)
	return p.Store.SaveCritic(ctx, w)
}
