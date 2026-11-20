// ebac-approve is the approve plugin of ebac: a sandboxed principal's
// APPROVE requests on bic/aether (approve-requests), and ORG-APPROVE's asks
// and tapped approvals (org-approve), each checked and posted with ebac's
// credential. It speaks sdk/plugin on stdin/stdout. Its only GHE write
// is an APPROVE review (internal/source/approve.GH).
package main

import (
	"context"

	"github.com/gluck/ebac/internal/source/approve"
	"github.com/gluck/ebac/internal/source/orgapprove"
	"github.com/gluck/ebac/sdk/model"
	"github.com/gluck/ebac/sdk/plugin"
)

func main() {
	plugin.Serve(func(ctx context.Context, req plugin.Request) (*plugin.Reply, error) {
		if req.Critic.SourceName() == model.SourceOrgApprove {
			return orgapprove.Plugin{}.Handle(ctx, req)
		}
		return approve.Plugin{}.Handle(ctx, req)
	})
}
