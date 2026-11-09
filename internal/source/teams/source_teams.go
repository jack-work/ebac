package teams

import (
	"context"
	"errors"
	"fmt"
	"html"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/gluck/ebac/internal/forge"
	"github.com/gluck/ebac/internal/form"
	"github.com/gluck/ebac/internal/model"
	"github.com/gluck/ebac/internal/plugin"
	"github.com/gluck/ebac/internal/tomb"
)

// teamsChat translates one Teams chat, read through tomb, into a critic's
// inbox. It has its own role form, timer and process, so a slow or dead tomb
// never delays a PR round.
//
// It is the ebac-tomb plugin.
type Plugin struct {
	// Tomb, when set, is used as is (tests); otherwise it is built from
	// EBAC_TOMB and EBAC_BAND_CRED.
	Tomb *tomb.Tomb
}

func (pl Plugin) Handle(ctx context.Context, req plugin.Request) (*plugin.Reply, error) {
	switch req.Op {
	case plugin.OpRound:
		tb := pl.Tomb
		if tb == nil {
			tb = &tomb.Tomb{Bin: env("EBAC_TOMB", "tomb"), Timeout: tomb.TeamsTombWait,
				Cred: forge.NewCred(env("EBAC_BAND_CRED", ""), forge.CredTeams)}
		}
		return round(ctx, tb, &req.Critic)
	case plugin.OpBrief:
		return &plugin.Reply{Brief: teamsBrief(&req.Critic, plugin.Unwire(req.Pending))}, nil
	}
	return nil, fmt.Errorf("op %q not supported", req.Op)
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
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
		// The source only translates: who wakes is the core's trigger rules.
		tier := model.TierWake
		if kind == "teams_deleted" {
			tier = model.TierRecord
		}
		who := m.Sender(st.Names)
		text := teamsText(m.Content)
		body, cut := model.CutBody(text, teamsBodyBytes)
		events = append(events, model.Event{Kind: model.EventKind(kind), Tier: tier, PR: model.PRKey(chat.Topic), Comment: m.ID, Author: who,
			AuthorID: m.SenderID(), Detail: model.FirstLine(body), Text: text})
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

func round(ctx context.Context, tb *tomb.Tomb, w *model.Critic) (*plugin.Reply, error) {
	rep := &plugin.Reply{Critic: w}
	if w.Chat == nil || w.Chat.ID == "" {
		return nil, fmt.Errorf("%s: teams critic without a chat id", w.Name)
	}
	if w.Runtime.Teams == nil {
		w.Runtime.Teams = &model.TeamsState{}
	}
	st := w.Runtime.Teams

	msgs, err := tb.Messages(ctx, w.Chat.ID, teamsLimit)
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
			teamsGone(rep, w, err)
			return rep, nil
		}
		teamsBlind(rep, w, err)
		rep.Error = err.Error()
		return rep, nil
	}
	st.GoneStreak, st.GoneSince = 0, ""
	if st.Blind {
		rep.Unset = append(rep.Unset, "blind")
		rep.Logf("%s: hearing the chat again (blind since %s)", w.Name, st.FailSince)
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
	rep.Inbox = items
	rep.Events = plugin.Wire(events)
	for i, e := range events {
		if _, ok := items["teams-"+e.Comment]; ok {
			rep.Events[i].Inbox = []string{"teams-" + e.Comment}
		}
	}
	return rep, nil
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
func teamsBlind(rep *plugin.Reply, w *model.Critic, cause error) {
	st := w.Runtime.Teams
	since, err := time.Parse(time.RFC3339, st.FailSince)
	if err != nil || time.Since(since) < teamsBlindAfter {
		return
	}
	if st.Blind && st.BlindErr == cause.Error() {
		return
	}
	rep.SetKey("blind", map[string]string{"since": st.FailSince, "last_error": cause.Error(), "updated": time.Now().UTC().Format(time.RFC3339),
		"meaning": "ebac cannot read the chat " + w.Chat.Topic + ": messages since then are unseen until this key disappears"})
	if !st.Blind {
		rep.Wake = append(rep.Wake, fmt.Sprintf("ebac %s: BLIND since %s: cannot read %q (%v). Messages are unseen until your form's key blind disappears; it clears itself on recovery. If it persists, the Boss may need to sign tomb in again.",
			w.Name, st.FailSince, w.Chat.Topic, cause))
		rep.Logf("%s: blind since %s: %v", w.Name, st.FailSince, cause)
	}
	st.Blind, st.BlindErr = true, cause.Error()
}

// teamsGone marks the chat gone on the form, wakes the seat once and stops.
func teamsGone(rep *plugin.Reply, w *model.Critic, cause error) {
	st := w.Runtime.Teams
	w.Runtime.Stopped = true
	w.Runtime.StopReason = fmt.Sprintf("chat gone since %s after %d consecutive answers: %v", st.GoneSince, st.GoneStreak, cause)
	action := "polling stopped. Delete this form and the critic (ebac rm --critic " + w.Name + " --purge). " +
		"If the chat moved to a new id, make a new critic with ebac add-teams: never repoint this one."
	rep.Gone = append(rep.Gone, plugin.Gone{Key: "chat", Resource: "teams chat " + w.Chat.ID + " (" + w.Chat.Topic + ")",
		Since: st.GoneSince, Error: cause.Error(), Action: action})
	rep.Wake = append(rep.Wake, fmt.Sprintf("ebac %s: the chat %q is gone (%v). Polling stopped; the form's key gone says what to do.", w.Name, w.Chat.Topic, cause))
	rep.DisableTimer, rep.Done = true, true
	rep.Logf("%s: %s", w.Name, w.Runtime.StopReason)
}
