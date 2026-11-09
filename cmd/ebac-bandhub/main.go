// ebac-bandhub is the band-hub plugin of ebac: a studied mirror of the band
// hub's queues, nodes and dupes, slim (warden orders 2026-11-09). It speaks
// internal/plugin on stdin/stdout.
package main

import (
	"github.com/gluck/ebac/internal/bandstate"
	"github.com/gluck/ebac/internal/plugin"
)

func main() { plugin.Serve(bandstate.Plugin{}.Handle) }
