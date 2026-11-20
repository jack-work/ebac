// ebac-roster is the roster plugin of ebac: it derives the figaro family's
// role registry from the aria list the core passes on stdin (aria-roster).
// It reaches nothing itself: no socket, no credential.
package main

import (
	"github.com/gluck/ebac/internal/rosterstate"
	"github.com/gluck/ebac/sdk/plugin"
)

func main() { plugin.Serve(rosterstate.Plugin{}.Handle) }
