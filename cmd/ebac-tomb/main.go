// ebac-tomb is the Teams plugin of ebac: one Teams chat read through tomb
// (teams-chat). It speaks internal/plugin on stdin/stdout.
package main

import (
	"github.com/gluck/ebac/internal/plugin"
	"github.com/gluck/ebac/internal/source/teams"
)

func main() { plugin.Serve(teams.Plugin{}.Handle) }
