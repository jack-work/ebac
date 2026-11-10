// ebac-gh is the GitHub plugin of ebac: pull request review (github-pr),
// author watches (github-author), release watches (github-releases) and board sync (github-prs). It speaks internal/plugin on stdin/stdout.
package main

import (
	"context"

	"github.com/gluck/ebac/internal/model"
	"github.com/gluck/ebac/internal/plugin"
	"github.com/gluck/ebac/internal/source/author"
	"github.com/gluck/ebac/internal/source/github"
	"github.com/gluck/ebac/internal/source/prboard"
	"github.com/gluck/ebac/internal/source/releases"
)

func main() {
	plugin.Serve(func(ctx context.Context, req plugin.Request) (*plugin.Reply, error) {
		if req.Critic.SourceName() == model.SourceBoard {
			return prboard.Plugin{}.Handle(ctx, req)
		}
		if req.Critic.SourceName() == model.SourceReleases {
			return releases.Plugin{}.Handle(ctx, req)
		}
		if req.Critic.SourceName() == "github-author" {
			return author.Plugin{}.Handle(ctx, req)
		}
		return github.Plugin{}.Handle(ctx, req)
	})
}
