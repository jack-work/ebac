package model

// SourceApprove is the approve path for a sandboxed principal that holds no
// GHE credential (GO-20261112-secretary-ahmed): it writes requests.<id> on
// the critic's inputs form, and the plugin (ebac-approve) posts an APPROVE
// review on bic/aether only after every check passes, writing
// requests.<id>.result. Its configuration lives on ebac's own role form,
// which the requester cannot write; the repo and the review event are code
// constants, not configuration.
const SourceApprove = "approve-requests"

// ApproveWatch is an approve-requests critic's configuration.
type ApproveWatch struct {
	// Chat is the Teams chat id the ask must be in; ChatAlias the value a
	// request's chat field must carry (e.g. "ahmed").
	Chat      string `json:"chat"`
	ChatAlias string `json:"chat_alias"`
	// AskFrom is the Teams sender id the ask must come from (8:orgid:<guid>).
	AskFrom string `json:"ask_from"`
	// Grant is the band-grants credential that must be live for Principal
	// (band-grants' principal name, as stored in grants.json).
	Grant     string `json:"grant"`
	Principal string `json:"principal"`
	// PingTo is the role (@form) told one line per approve.
	PingTo string `json:"ping_to"`
}
