package model

import (
	"reflect"
	"strings"
	"testing"
)

// Every authority field: a form value that differs from the trusted one is replaced and named; an equal one is not
// named. RED if any field is read from the form again.
func TestAuthorityApplyEveryField(t *testing.T) {
	trusted := Authority{Source: SourceApprove, Mode: ModeReview, Write: false, Approve: false, Grant: "ebac-approve.aether", Principal: "sbx-ops-sec",
		AskFrom: "8:orgid:a", Chat: "19:c", ChatAlias: "ahmed", InputsForm: "@in", BoardForm: "@board"}
	form := &Critic{Source: SourceOrgApprove, Mode: ModeReviewer, Write: true, Approve: true, InputsForm: "@evil", BoardForm: "@evil2",
		Requests: &ApproveWatch{Grant: "ebac-approve.org", Principal: "org-approve", AskFrom: "8:orgid:evil", Chat: "19:evil", ChatAlias: "eve"}}
	got := strings.Join(trusted.Apply(form), ",")
	want := "source,mode,write,approve,inputs_form,board_form,approve_requests.grant,approve_requests.principal,approve_requests.ask_from,approve_requests.chat,approve_requests.chat_alias"
	if got != want {
		t.Fatalf("ignored\n got %s\nwant %s", got, want)
	}
	if !reflect.DeepEqual(AuthorityOf(form), trusted) {
		t.Fatalf("after apply %+v, want %+v", AuthorityOf(form), trusted)
	}
	if again := trusted.Apply(form); len(again) != 0 {
		t.Fatalf("equal values flagged: %v", again)
	}
	org := &Critic{Source: SourceOrgApprove, OrgApprove: &OrgApproveWatch{Grant: "x", Principal: "y", Approver: "eve"}}
	got = strings.Join(Authority{Source: SourceOrgApprove, Grant: "ebac-approve.org", Principal: "org-approve", Approver: "jack"}.Apply(org), ",")
	if got != "org_approve.grant,org_approve.principal,org_approve.approver" || org.OrgApprove.Approver != "jack" {
		t.Fatalf("org-approve: %s %+v", got, org.OrgApprove)
	}
}

// What ebac records when it creates a critic: source and mode only; reviewer (which posts without write) as review;
// never write, approve, grants or outside forms (review f0f3a1d9 B1: any seat can run ebac add). RED if any of
// these reaches the record.
func TestCreationAuthorityIsLeast(t *testing.T) {
	w := &Critic{Source: SourceOrgApprove, Mode: ModeReviewer, Write: true, Approve: true, InputsForm: "@in",
		OrgApprove: &OrgApproveWatch{Grant: "g", Principal: "p", Approver: "eve"}}
	if got := CreationAuthority(w); !reflect.DeepEqual(got, Authority{Source: SourceOrgApprove, Mode: ModeReview}) {
		t.Fatalf("%+v", got)
	}
}

// The read-target fields (review 1b46af66 (3)): a teams chat id and org-approve logins/known breaks from the form are
// ignored and named; creation records them (they are what the creator asked it to read). RED if any arm goes.
func TestAuthorityReadTargets(t *testing.T) {
	a := Authority{Source: "teams-chat", Mode: ModeTeams, TeamsChat: "19:boss"}
	w := &Critic{Source: "teams-chat", Mode: ModeTeams, Chat: &ChatRef{ID: "19:someone-else"}}
	if got := strings.Join(a.Apply(w), ","); got != "chat.id" || w.Chat.ID != "19:boss" {
		t.Fatalf("teams: %s %q", got, w.Chat.ID)
	}
	o := Authority{Source: SourceOrgApprove, Logins: []string{"a"}, KnownBreaks: []string{"ci"}}
	ow := &Critic{Source: SourceOrgApprove, OrgApprove: &OrgApproveWatch{Logins: []string{"a", "evil"}, KnownBreaks: []string{"ci", "required-check"}}}
	if got := strings.Join(o.Apply(ow), ","); got != "org_approve.logins,org_approve.known_breaks" ||
		!reflect.DeepEqual(ow.OrgApprove.Logins, []string{"a"}) || !reflect.DeepEqual(ow.OrgApprove.KnownBreaks, []string{"ci"}) {
		t.Fatalf("org: %s %+v", got, ow.OrgApprove)
	}
	c := CreationAuthority(&Critic{Source: "teams-chat", Mode: ModeTeams, Chat: &ChatRef{ID: "19:x"}, Write: true})
	if c.TeamsChat != "19:x" || c.Write {
		t.Fatalf("creation: %+v", c)
	}
}

// #45: the ours login comes from the trusted config only (a form's gh_write is replaced), alone or beside a write
// scope, and reads back through AuthorityOf. RED if Apply drops it or reads it from the form.
func TestOursLoginIsTrustedOnly(t *testing.T) {
	form := &Critic{Source: SourceGHCache, GHWrite: &GHWrite{Ours: "intruder"}}
	ignored := Authority{Source: SourceGHCache, OursLogin: "ours"}.Apply(form)
	if form.GHWrite == nil || form.GHWrite.Ours != "ours" || !strings.Contains(strings.Join(ignored, ","), "gh_write") {
		t.Fatalf("ours alone: %+v, ignored %v", form.GHWrite, ignored)
	}
	scoped := &Critic{Source: SourceGHCache}
	Authority{Source: SourceGHCache, Scope: &GHScope{Repos: []string{"example/repo"}}, WriteFields: []string{FieldLabels}, OursLogin: "ours"}.Apply(scoped)
	if scoped.GHWrite == nil || scoped.GHWrite.Ours != "ours" || AuthorityOf(scoped).OursLogin != "ours" {
		t.Fatalf("ours beside a scope: %+v", scoped.GHWrite)
	}
	none := &Critic{Source: SourceGHCache, GHWrite: &GHWrite{Ours: "intruder"}}
	Authority{Source: SourceGHCache}.Apply(none)
	if none.GHWrite != nil {
		t.Fatalf("no trusted ours: %+v", none.GHWrite)
	}
}
