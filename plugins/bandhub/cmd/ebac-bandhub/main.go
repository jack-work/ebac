// ebac-bandhub is the band-hub plugin of ebac: a studied mirror of the band
// hub's queues, nodes and dupes, slim (gatekeeper orders 2026-11-09). It speaks
// sdk/plugin on stdin/stdout.
package main

import (
	"github.com/gluck/ebac/plugins/bandhub/internal/bandstate"
	"github.com/gluck/ebac/sdk/plugin"
)

func main() {
	plugin.Serve(plugin.Info{Name: "bandhub", Sources: []string{"band-hub"}}, bandstate.Plugin{}.Handle)
}
