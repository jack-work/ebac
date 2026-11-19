package model

import (
	"strings"
	"testing"
)

// Every authority field: a form value that differs from the trusted one is replaced and named; an equal one is not
// named. RED if any field is read from the form again.
func TestAuthorityApplyEveryField(t *testing.T) {
	trusted := Authority{Source: SourceApprove, Mode: ModeReview, Write: false, Approve: false, Grant: "ebac-approve.aether", Principal: "sbx-cheroot-sec",
		AskFrom: "8:orgid:a", Chat: "19:c", ChatAlias: "ahmed", InputsForm: "@in", BoardForm: "@board"}
	form := &Critic{Source: SourceOrgApprove, Mode: ModeReviewer, Write: true, Approve: true, InputsForm: "@evil", BoardForm: "@evil2",
		Requests: &ApproveWatch{Grant: "ebac-approve.org", Principal: "org-approve", AskFrom: "8:orgid:evil", Chat: "19:evil", ChatAlias: "eve"}}
	got := strings.Join(trusted.Apply(form), ",")
	want := "source,mode,write,approve,inputs_form,board_form,approve_requests.grant,approve_requests.principal,approve_requests.ask_from,approve_requests.chat,approve_requests.chat_alias"
	if got != want {
		t.Fatalf("ignored\n got %s\nwant %s", got, want)
	}
	if AuthorityOf(form) != trusted {
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
	if got := CreationAuthority(w); got != (Authority{Source: SourceOrgApprove, Mode: ModeReview}) {
		t.Fatalf("%+v", got)
	}
}
