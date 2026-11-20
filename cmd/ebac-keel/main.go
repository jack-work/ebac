// ebac-keel is the keel plugin of ebac: one keel repo's comment threads
// (keel-threads) and its work branches (keel-branches). It speaks
// sdk/plugin on stdin/stdout.
package main

import (
	"context"

	"github.com/gluck/ebac/internal/source/keel"
	"github.com/gluck/ebac/internal/source/keelbranch"
	"github.com/gluck/ebac/sdk/model"
	"github.com/gluck/ebac/sdk/plugin"
)

func main() {
	plugin.Serve(func(ctx context.Context, req plugin.Request) (*plugin.Reply, error) {
		if req.Critic.SourceName() == model.SourceKeelBranches {
			return keelbranch.Plugin{}.Handle(ctx, req)
		}
		return keel.Plugin{}.Handle(ctx, req)
	})
}
