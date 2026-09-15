package main

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// Succession lives HERE, in the binary, not in ~/.config/figaro/outfits.
//
// An outfit is user config: it can be edited, renamed or deleted between the
// round that reads it and the round that acts on it, and a critic whose
// succession policy vanished mid-assignment fails in the least visible way
// possible — it simply never hands over, and the first symptom is a seat that
// has stopped answering. The policy is shipped, versioned, and written onto
// the role form as a birth patch so the seat can read the terms it is being
// held to.
//
// The thresholds are FRACTIONS of the holder's context window, not
// percentages, and they are ceilings: act at them or before, never after.

type SuccessionPolicy struct {
	PolicyVersion  int      `json:"policy_version"`
	StandbyAt      float64  `json:"standby_context_fraction"`
	RecastAt       float64  `json:"recast_context_fraction"`
	ForkTurn       int      `json:"fork_turn"`
	StandbyOutfit  string   `json:"standby_outfit"`
	Instructions   string   `json:"instructions"`
	DivisibleTasks []string `json:"divisible_tasks"`
}

func DefaultSuccession() SuccessionPolicy {
	return SuccessionPolicy{
		PolicyVersion: 1,
		StandbyAt:     0.70,
		RecastAt:      0.80,
		// Fork at an EARLY turn, not at the head. A fork inherits the
		// parent's context, so forking a 70%-full holder mints a
		// 70%-full successor — which is the problem, not the fix. Turn 1
		// keeps the shared prefix (and its warm provider cache) without
		// the accumulated transcript.
		ForkTurn:      1,
		StandbyOutfit: "sonnet",
		Instructions:  "Prepare a successor at 70% context; cast it into this role at 80%. Callers keep addressing the role id, so nothing they do changes.",
		// Deliberately read-only and divisible: they warm the successor
		// without letting two holders answer the same thread. Never two
		// holders — if you are unsure who holds the work, you do not.
		DivisibleTasks: []string{
			"Read the role form: its critic, its PRs, the current delta, and the harness contract.",
			"Inventory the standing unresolved threads read-only. Do not reply to any of them.",
			"Prepare the verification commands you would run on the next delta, and a one-paragraph status summary.",
			"Post nothing, edit nothing, push nothing, ack nothing until you are cast.",
		},
	}
}

// SuccessionState is what the supervisor records on the role form. The seat
// studies the role, so it sees its own succession clock move.
type SuccessionState struct {
	Fill       float64 `json:"context_fill"`
	Stage      string  `json:"stage"` // primary | standby-prepared | recast
	Generation int     `json:"generation"`
	Holder     string  `json:"holder"`
	Standby    string  `json:"standby,omitempty"`
	StandbyAt  string  `json:"standby_minted_at,omitempty"`
	LastRecast string  `json:"last_recast_at,omitempty"`
	Note       string  `json:"note,omitempty"`
}

// successionAction is the decision, isolated from the doing.
//
// The branches that MATTER here are the ones that almost never run: a
// threshold crossing happens once per seat lifetime, so the fire path would
// otherwise ship untested and be exercised for the first time on a live
// assignment at 80% context. Table-test the decision; integration-test the
// effects.
type succAction string

const (
	succNone   succAction = "none"
	succMint   succAction = "mint-standby"
	succRecast succAction = "recast"
	succRemint succAction = "standby-lost"
)

func successionAction(fill float64, standby string, standbyAlive bool, pol SuccessionPolicy) succAction {
	switch {
	case fill >= pol.RecastAt && standby != "" && standbyAlive:
		return succRecast
	case fill >= pol.StandbyAt && standby == "":
		return succMint
	case standby != "" && !standbyAlive:
		return succRemint
	}
	return succNone
}

// superviseSuccession runs once per poll round. It is deliberately part of
// the existing heartbeat rather than a second daemon: the timer already runs
// every 5m and already knows the seat.
func (p *Poller) superviseSuccession(ctx context.Context, c *Critic) {
	pol := DefaultSuccession()

	form, err := p.Fig.Form(ctx, c.FormID)
	if err != nil {
		return
	}
	holder, _ := form["target-aria"].(string)
	if holder == "" {
		return
	}

	fill, tokens, limit, err := p.Fig.AriaFill(ctx, holder)
	if err != nil {
		// Report the fields, not just the failure: a fill we could not
		// measure and a fill of zero must not look the same.
		_ = p.Fig.SetJSON(ctx, c.FormID, "succession_state", SuccessionState{
			Holder: holder, Stage: "unmeasured", Generation: c.Runtime.Generation,
			Standby: c.Runtime.Standby,
			Note:    fmt.Sprintf("context_tokens=%d context_limit=%d: %v", tokens, limit, err),
		})
		return
	}

	state := SuccessionState{
		Fill: round2(fill), Stage: "primary", Generation: c.Runtime.Generation,
		Holder: holder, Standby: c.Runtime.Standby, StandbyAt: c.Runtime.StandbyMintedAt,
	}

	switch {
	// RECAST. The standby has had at least one round to answer its
	// preparation turn; move the seat.
	case fill >= pol.RecastAt && c.Runtime.Standby != "" && p.Fig.AriaExists(ctx, c.Runtime.Standby):
		standby := c.Runtime.Standby
		if err := p.Fig.Cast(ctx, standby, c.FormID); err != nil {
			state.Stage, state.Note = "standby-prepared", "recast FAILED: "+err.Error()
			break
		}
		c.Runtime.Generation++
		c.Runtime.Standby = ""
		c.Runtime.StandbyMintedAt = ""
		c.Runtime.LastRecast = time.Now().UTC().Format(time.RFC3339)
		_ = p.Store.SaveCritic(c)

		state.Stage = "recast"
		state.Generation = c.Runtime.Generation
		state.Holder = standby
		state.Standby = ""
		state.LastRecast = c.Runtime.LastRecast
		state.Note = fmt.Sprintf("seat moved %s -> %s at %.0f%% fill", holder, standby, fill*100)

		fmt.Fprintf(p.Out, "%s: SUCCESSION gen %d — %s -> %s at %.0f%%\n",
			c.Name, c.Runtime.Generation, holder, standby, fill*100)

		// Tell the outgoing holder, by aria id and not through the role:
		// the role already points at its successor.
		_ = p.Fig.SendTo(ctx, holder, fmt.Sprintf(
			"Stand down. You have handed critic %q to %s at %.0f%% context; the role %s now points at it.\n"+
				"You are the advisor, not the holder. Touch nothing: no acks, no posts, no edits.\n"+
				"Answer its questions fully — being asked is now your job.",
			c.Name, standby, fill*100, c.FormID))

	// STANDBY. Mint one and give it the read-only warm-up.
	case fill >= pol.StandbyAt && c.Runtime.Standby == "":
		brief := standbyBrief(c, pol, holder, fill)
		standby, err := p.Fig.ForkAt(ctx, holder, pol.ForkTurn, pol.StandbyOutfit, brief)
		if err != nil {
			state.Note = "standby fork FAILED: " + err.Error()
			break
		}
		c.Runtime.Standby = standby
		c.Runtime.StandbyMintedAt = time.Now().UTC().Format(time.RFC3339)
		_ = p.Store.SaveCritic(c)

		state.Stage = "standby-prepared"
		state.Standby = standby
		state.StandbyAt = c.Runtime.StandbyMintedAt
		state.Note = fmt.Sprintf("standby minted at %.0f%% fill; recast at %.0f%%", fill*100, pol.RecastAt*100)
		fmt.Fprintf(p.Out, "%s: standby %s minted at %.0f%% fill\n", c.Name, standby, fill*100)

	// The standby was reaped out from under us. Say so; the next round
	// mints another.
	case c.Runtime.Standby != "" && !p.Fig.AriaExists(ctx, c.Runtime.Standby):
		state.Note = "standby " + c.Runtime.Standby + " no longer resolves; will re-mint"
		c.Runtime.Standby = ""
		c.Runtime.StandbyMintedAt = ""
		_ = p.Store.SaveCritic(c)
		state.Standby = ""

	case c.Runtime.Standby != "":
		state.Stage = "standby-prepared"
	}

	_ = p.Fig.SetJSON(ctx, c.FormID, "succession", pol)
	_ = p.Fig.SetJSON(ctx, c.FormID, "succession_state", state)
}

func standbyBrief(c *Critic, pol SuccessionPolicy, holder string, fill float64) string {
	var b strings.Builder
	fmt.Fprintf(&b, "You are the STANDBY successor for ebac critic %q.\n\n", c.Name)
	fmt.Fprintf(&b, "The current holder (%s) is at %.0f%% of its context. At %.0f%% the role %s "+
		"will be re-pointed at you, automatically, by ebac. You are not a subagent and you do not "+
		"report to the holder — you replace it.\n\n", holder, fill*100, pol.RecastAt*100, c.FormID)
	b.WriteString("Until you are cast, do exactly this and nothing more:\n")
	for i, t := range pol.DivisibleTasks {
		fmt.Fprintf(&b, "  %d. %s\n", i+1, t)
	}
	fmt.Fprintf(&b, "\nRead the role with `figaro form %s -j`. Values there are elided at %d chars "+
		"with a trailing …; re-read rather than reporting on a cut value.\n", c.FormID, formValueElisionLimit)
	b.WriteString("\nNEVER TWO HOLDERS. Until the cast lands, the other aria answers. If you are " +
		"unsure whether you hold this critic, you do not.\n")
	return b.String()
}

func round2(f float64) float64 { return float64(int(f*100+0.5)) / 100 }
