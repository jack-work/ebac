package model

import (
	"fmt"
	"slices"
)

// Authority is every critic field that decides what ebac DOES on someone's
// behalf (#11; reviews e025e175 H1, 6ae24e1e): which plugin and sandbox
// grants it runs with (Source), its mode, whether it may write or approve,
// the band-grants credential and principal its approvals need and who may
// tap them, where approval asks may come from, and the outside forms it
// writes. A critic's FORM is seat-writable, so these come only from ebac's
// trusted configuration (keyed by role form id) or from ebac's own record
// made when the critic was created; a form value is ignored and flagged.
type Authority struct {
	Source     string `json:"source"`
	Mode       Mode   `json:"mode"`
	Write      bool   `json:"write,omitempty"`
	Approve    bool   `json:"approve,omitempty"`
	Grant      string `json:"grant,omitempty"`
	Principal  string `json:"principal,omitempty"`
	Approver   string `json:"approver,omitempty"`
	AskFrom    string `json:"ask_from,omitempty"`
	Chat       string `json:"chat,omitempty"`
	ChatAlias  string `json:"chat_alias,omitempty"`
	InputsForm string `json:"inputs_form,omitempty"`
	BoardForm  string `json:"board_form,omitempty"`
	// TeamsChat is the Teams conversation a teams critic reads with ebac's
	// Teams credential; Logins and KnownBreaks are whose PRs an org-approve
	// critic files approval asks for, and which red checks count as green
	// (review 1b46af66 (3)).
	TeamsChat   string   `json:"teams_chat,omitempty"`
	Logins      []string `json:"logins,omitempty"`
	KnownBreaks []string `json:"known_breaks,omitempty"`
}

// AuthorityOf reads a critic's authority fields.
func AuthorityOf(w *Critic) Authority {
	a := Authority{Source: w.Source, Mode: w.Mode, Write: w.Write, Approve: w.Approve, InputsForm: w.InputsForm, BoardForm: w.BoardForm}
	if r := w.Requests; r != nil {
		a.Grant, a.Principal, a.AskFrom, a.Chat, a.ChatAlias = r.Grant, r.Principal, r.AskFrom, r.Chat, r.ChatAlias
	}
	if o := w.OrgApprove; o != nil {
		a.Grant, a.Principal, a.Approver = o.Grant, o.Principal, o.Approver
		a.Logins, a.KnownBreaks = slices.Clone(o.Logins), slices.Clone(o.KnownBreaks)
	}
	if c := w.Chat; c != nil {
		a.TeamsChat = c.ID
	}
	return a
}

// Apply sets w's authority fields to a and returns the names of the fields
// whose form value differed (ignored; ebac ls flags them).
func (a Authority) Apply(w *Critic) []string {
	var ignored []string
	set := func(name string, form, trusted string, dst *string) {
		if form != trusted {
			ignored = append(ignored, name)
		}
		*dst = trusted
	}
	setb := func(name string, form, trusted bool, dst *bool) {
		if form != trusted {
			ignored = append(ignored, name)
		}
		*dst = trusted
	}
	set("source", w.Source, a.Source, &w.Source)
	if w.Mode != a.Mode {
		ignored = append(ignored, "mode")
	}
	w.Mode = a.Mode
	setb("write", w.Write, a.Write, &w.Write)
	setb("approve", w.Approve, a.Approve, &w.Approve)
	set("inputs_form", w.InputsForm, a.InputsForm, &w.InputsForm)
	set("board_form", w.BoardForm, a.BoardForm, &w.BoardForm)
	if r := w.Requests; r != nil {
		set("approve_requests.grant", r.Grant, a.Grant, &r.Grant)
		set("approve_requests.principal", r.Principal, a.Principal, &r.Principal)
		set("approve_requests.ask_from", r.AskFrom, a.AskFrom, &r.AskFrom)
		set("approve_requests.chat", r.Chat, a.Chat, &r.Chat)
		set("approve_requests.chat_alias", r.ChatAlias, a.ChatAlias, &r.ChatAlias)
	}
	if o := w.OrgApprove; o != nil {
		set("org_approve.grant", o.Grant, a.Grant, &o.Grant)
		set("org_approve.principal", o.Principal, a.Principal, &o.Principal)
		set("org_approve.approver", o.Approver, a.Approver, &o.Approver)
		if !slices.Equal(o.Logins, a.Logins) {
			ignored = append(ignored, "org_approve.logins")
		}
		if !slices.Equal(o.KnownBreaks, a.KnownBreaks) {
			ignored = append(ignored, "org_approve.known_breaks")
		}
		o.Logins, o.KnownBreaks = slices.Clone(a.Logins), slices.Clone(a.KnownBreaks)
	}
	if c := w.Chat; c != nil {
		set("chat.id", c.ID, a.TeamsChat, &c.ID)
	}
	return ignored
}

// NoAuthorityNote is the ebac ls health of a critic with no trusted
// authority: it is not run (fail closed).
func NoAuthorityNote(formID string) string {
	return fmt.Sprintf("no trusted authority for %s: not run (add it to ebac's trusted config)", formID)
}

// CreationAuthority is what ebac records for a critic it creates: its source
// and mode (reviewer mode, which posts without write, is recorded as review),
// and what it reads (teams chat, org-approve logins and known breaks), and
// nothing else. Write, approve, approval grants and outside forms come
// only from the trusted config (review f0f3a1d9 B1).
func CreationAuthority(w *Critic) Authority {
	mode := w.Mode
	if mode == ModeReviewer {
		mode = ModeReview
	}
	a := Authority{Source: w.Source, Mode: mode}
	// What a critic reads (not what it may do): set by whoever creates it, as
	// add-teams and add-org-approve always could; a later form edit is ignored.
	if c := w.Chat; c != nil {
		a.TeamsChat = c.ID
	}
	if o := w.OrgApprove; o != nil {
		a.Logins, a.KnownBreaks = slices.Clone(o.Logins), slices.Clone(o.KnownBreaks)
	}
	return a
}
