// ebac-tomb is the Teams plugin of ebac: one Teams chat read through tomb
// (teams-chat). It speaks sdk/plugin on stdin/stdout.
package main

import (
	"github.com/gluck/ebac/internal/source/teams"
	"github.com/gluck/ebac/sdk/plugin"
)

func main() { plugin.Serve(teams.Plugin{}.Handle) }
