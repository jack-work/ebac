package model

// SourceOrgApprove is ORG-APPROVE (the Boss, 2026-11-17): the open bic/aether
// pull requests of the Boss's org, read with one lean search per round,
// scored for "stalled, ready for an approval"; each candidate is asked on an
// elicitation board (the critic's inputs form, q.oa-*) and filed as a
// band-grants request (principal org-approve, credential ebac-approve.org)
// whose /grants/ approval by jack IS the tap: ebac posts the APPROVE only for
// a request it filed, approved by jack, live, at the head it scored.
const SourceOrgApprove = "org-approve"

// OrgApproveWatch is an org-approve critic's configuration.
type OrgApproveWatch struct {
	// Logins are the GHE logins whose open PRs are read (the org chart).
	Logins []string `json:"logins"`
	// Grant and Principal: the band-grants credential each tap request is
	// filed for, as Principal (ebac's uid maps to it on the request socket).
	Grant     string `json:"grant"`
	Principal string `json:"principal"`
	// Approver is the only approved_by a tap is taken from.
	Approver string `json:"approver"`
	// PingTo is the role (@form) told one line per new ask and approval.
	PingTo string `json:"ping_to,omitempty"`
	// KnownBreaks are check names allowed red (main-wide known breaks).
	KnownBreaks []string `json:"known_breaks,omitempty"`
}

// OrgApproveState is what the critic keeps between rounds.
type OrgApproveState struct {
	// PRs are the scored open PRs, by number (decimal string).
	PRs map[string]*OrgPR `json:"prs,omitempty"`
	// Asks are the tap requests ebac filed, by q id (oa-<pr>-<head7>).
	Asks map[string]*OrgAsk `json:"asks,omitempty"`
}

// OrgPR is one scored pull request.
type OrgPR struct {
	Number  int      `json:"n"`
	Author  string   `json:"author"`
	Title   string   `json:"title"`
	Head    string   `json:"head"`
	DeepAt  string   `json:"deep_at,omitempty"` // RFC3339 of the last deep read
	Score   int      `json:"score"`
	Fails   []string `json:"fails,omitempty"`
	Extra   []string `json:"extra,omitempty"` // excluded: CONFLICTING, already APPROVED, Jack reviewed
	Sens    []string `json:"sens,omitempty"`  // sensitive paths (never auto)
	Reasons []string `json:"reasons,omitempty"`
}

// Candidate: all six criteria met and nothing excluding it.
func (p *OrgPR) Candidate() bool { return p.Score == 6 && len(p.Extra) == 0 && p.DeepAt != "" }

// OrgAsk is one filed tap request.
type OrgAsk struct {
	PR        int    `json:"pr"`
	Head      string `json:"head"`
	RequestID string `json:"request_id,omitempty"` // band-grants request id; "" until filed
	FiledAt   string `json:"filed_at,omitempty"`
	State     string `json:"state"` // open, approved, withdrawn, lapsed
	ReviewID  int64  `json:"review_id,omitempty"`
	// Post is the ask's post ledger (#27 B): set when the approve is
	// prepared, PostedAt when it was sent.
	Post *PostRecord `json:"post,omitempty"`
}

// PostRecord is one prepared approve (#27 design B). Token is the round's
// CommitToken it was prepared under: it is posted only by that round's
// OpCommit, after the core saved it; a record whose token passed unposted is
// never posted (only read back). PostedAt is set once the POST was sent
// (also when it reported an error: it may have landed).
type PostRecord struct {
	PR         int    `json:"pr"`
	Marker     string `json:"marker"`
	Token      string `json:"token"`
	PreparedAt string `json:"prepared_at"`
	PostedAt   string `json:"posted_at,omitempty"`
}
