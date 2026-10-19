package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func protocolCritics() []Critic {
	return []Critic{
		{Name: "ro", Mode: ModeReview, FormID: "@f"},
		{Name: "rw", Mode: ModeReview, Write: true, FormID: "@f"},
		{Name: "dev", Mode: ModeDevelop, IssueRepo: "acme/ebac", FormID: "@f"},
		{Name: "rev", Mode: ModeReviewer, Host: "ghe.example", Approve: true, FormID: "@f",
			Worktree: &Worktree{Clone: "/src/widget", Root: "/wt"}},
	}
}

func TestBriefCarriesTheDeltaNotTheProtocol(t *testing.T) {
	t.Setenv("EBAC_SIGNATURE", filepath.Join(t.TempDir(), "absent"))
	pr := &PRState{Key: MakePRKey("acme", "widget", 7), Number: 7, State: "OPEN", Author: "friend", HeadSHA: "abcdef1234567"}
	events := []Event{{Kind: EvHeadMoved, Tier: TierWake, PR: pr.Key}}
	for _, w := range protocolCritics() {
		w.Runtime.Round = 42
		brief := buildBrief(&w, snapWith(pr), events, "jack", 0)
		for _, banned := range []string{"BOUNDARY", "YOUR JOB", "HOW TO ACT", "SET THE PROTOCOL", "SIGNAL, DO NOT NARRATE", "SIGNATURE"} {
			if strings.Contains(brief, banned) {
				t.Errorf("%s: brief repeats protocol section %q", w.Name, banned)
			}
		}
		for _, want := range []string{
			"WORTH YOUR ATTENTION\n",
			"STANDING\n  acme/widget#7 OPEN",
			"Protocol: form @f keys harness, harness_protocol_1..",
			"When finished: ebac ack --critic " + w.Name + " --round 42",
		} {
			if !strings.Contains(brief, want) {
				t.Errorf("%s: brief lacks %q:\n%s", w.Name, want, brief)
			}
		}
	}
}

func TestProtocolChunksFitTheStudyCut(t *testing.T) {
	t.Setenv("EBAC_SIGNATURE", filepath.Join(t.TempDir(), "absent"))
	pr := &PRState{Key: MakePRKey("acme", "widget", 7), Number: 7, State: "OPEN", Author: "friend"}
	texts := map[string]string{
		"one line longer than the cut, quoted and multibyte": strings.Repeat(`"—" `, 900) + "\nend\n",
	}
	for _, w := range protocolCritics() {
		texts[w.Name] = protocolText(&w, snapWith(pr), "jack")
	}
	if len(texts["rw"]) <= formValueElisionLimit {
		t.Fatalf("rw protocol is %d bytes; the split is untested below the cut", len(texts["rw"]))
	}
	for name, text := range texts {
		chunks := protocolChunks(text)
		if got := strings.Join(chunks, ""); got != text {
			t.Errorf("%s: chunks do not reassemble the protocol (%d vs %d bytes)", name, len(got), len(text))
		}
		for i, c := range chunks {
			if n := jsonLen(c); n > formValueElisionLimit {
				t.Errorf("%s: chunk %d is %d JSON bytes, over the %d cut", name, i+1, n, formValueElisionLimit)
			}
		}
		if len(text) > formValueElisionLimit && len(chunks) < 2 {
			t.Errorf("%s: %d bytes in one chunk", name, len(text))
		}
	}
}

func TestLiveProtocolProjectionDropsStaleChunks(t *testing.T) {
	f := liveOrSkip(t)
	ctx := context.Background()
	id, err := f.FormNew(ctx, "ebac-livetest-protocol")
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	t.Cleanup(func() { _ = f.FormRemove(ctx, id) })

	if err := projectProtocol(ctx, f, id, []string{"a\n", "b\n", "c\n"}); err != nil {
		t.Fatalf("project 3: %v", err)
	}
	form, _, err := f.FormRaw(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := form["harness_protocol_3"]; !ok {
		t.Fatalf("positive control: chunk 3 never written: %v", form)
	}
	if err := projectProtocol(ctx, f, id, []string{"only\n"}); err != nil {
		t.Fatalf("project 1: %v", err)
	}
	got, err := f.Form(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if got["harness_protocol_1"] != "only\n" {
		t.Errorf("chunk 1 = %#v", got["harness_protocol_1"])
	}
	for _, k := range []string{"harness_protocol_2", "harness_protocol_3"} {
		if _, ok := got[k]; ok {
			t.Errorf("stale %s survived", k)
		}
	}
}
