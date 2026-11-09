// ebac-gh is the GitHub plugin of ebac: pull request review (github-pr) and
// author watches (github-author). It speaks internal/plugin on stdin/stdout.
package main

import (
	"context"

	"github.com/gluck/ebac/internal/plugin"
	"github.com/gluck/ebac/internal/source/author"
	"github.com/gluck/ebac/internal/source/github"
)

func main() {
	plugin.Serve(func(ctx context.Context, req plugin.Request) (*plugin.Reply, error) {
		if req.Critic.SourceName() == "github-author" {
			return author.Plugin{}.Handle(ctx, req)
		}
		return github.Plugin{}.Handle(ctx, req)
	})
}
