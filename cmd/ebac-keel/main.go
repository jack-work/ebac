// ebac-keel is the keel plugin of ebac: one keel repo's comment threads
// (keel-threads) and its work branches (keel-branches). It speaks
// internal/plugin on stdin/stdout.
package main

import (
	"context"

	"github.com/gluck/ebac/internal/model"
	"github.com/gluck/ebac/internal/plugin"
	"github.com/gluck/ebac/internal/source/keel"
	"github.com/gluck/ebac/internal/source/keelbranch"
)

func main() {
	plugin.Serve(func(ctx context.Context, req plugin.Request) (*plugin.Reply, error) {
		if req.Critic.SourceName() == model.SourceKeelBranches {
			return keelbranch.Plugin{}.Handle(ctx, req)
		}
		return keel.Plugin{}.Handle(ctx, req)
	})
}
