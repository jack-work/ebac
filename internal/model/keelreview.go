package model

// Reviews on keel (see the keel source): the review key and the verdicts,
// shared by the CLI that stamps them, the core that verifies them and the
// plugin that derives review_state.

// KeelReviewConfig is written only by `ebac review` (stamped).
type KeelReviewConfig struct {
	Doc       string   `json:"doc"`
	Reviewers []string `json:"reviewers,omitempty"`
	// AuthorRole is the doc's author side: its holder (and forks) may not
	// record a verdict.
	AuthorRole string `json:"author_role,omitempty"`
	ByAria     string `json:"by_aria,omitempty"`
	At         string `json:"at,omitempty"`
	Stamp      string `json:"stamp,omitempty"`
}

// KeelConfigStampFields are the stamped fields of a review key, in order.
func KeelConfigStampFields(formID, critic, repo string, c KeelReviewConfig) []string {
	return append([]string{"review", formID, critic, repo, c.Doc, c.AuthorRole, c.ByAria, c.At}, c.Reviewers...)
}

// KeelVerdict is written only by `ebac verdict` (stamped).
type KeelVerdict struct {
	Verdict string `json:"verdict"`
	Rev     string `json:"rev,omitempty"`
	At      string `json:"at,omitempty"`
	ByAria  string `json:"by_aria,omitempty"`
	Note    string `json:"note,omitempty"`
	// Repo, Doc and RoleForm bind the verdict: a pass on one doc, repo or
	// role form never counts for another.
	Repo     string `json:"repo,omitempty"`
	Doc      string `json:"doc,omitempty"`
	RoleForm string `json:"role_form,omitempty"`
	Stamp    string `json:"stamp,omitempty"`
}

// KeelVerdictStampFields are the stamped fields of a verdict, in order.
func KeelVerdictStampFields(formID, critic, role string, v KeelVerdict) []string {
	return []string{"verdict", formID, critic, v.Repo, v.Doc, v.Rev, role, v.RoleForm, v.Verdict, v.At, v.ByAria, v.Note}
}

// KeelReviewCheckKey is the request key the core fills for a keel review.
const KeelReviewCheckKey = "review_check"

// KeelReviewCheck is the core's verification, per round.
type KeelReviewCheck struct {
	// Config: the review key's stamp verifies over this critic.
	Config bool `json:"config"`
	// Roles: per named reviewer, its verdict counts (stamp verifies, bound to
	// this repo and doc, to the role's current form and current holder);
	// Why says why not.
	Roles map[string]KeelRoleCheck `json:"roles,omitempty"`
}

type KeelRoleCheck struct {
	Valid bool   `json:"valid"`
	Why   string `json:"why,omitempty"`
}
