// ebac-gh is the GitHub plugin of ebac: pull request review (github-pr),
// author watches (github-author), release watches (github-releases) and the scout (github-scout). It speaks sdk/plugin on stdin/stdout.
package main

import (
	"context"

	"github.com/gluck/ebac/plugins/github/internal/author"
	"github.com/gluck/ebac/plugins/github/internal/github"
	"github.com/gluck/ebac/plugins/github/internal/releases"
	"github.com/gluck/ebac/plugins/github/internal/scout"
	"github.com/gluck/ebac/sdk/model"
	"github.com/gluck/ebac/sdk/plugin"
)

func main() {
	plugin.Serve(func(ctx context.Context, req plugin.Request) (*plugin.Reply, error) {
		if req.Critic.SourceName() == model.SourceScout {
			return scout.Plugin{}.Handle(ctx, req)
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
