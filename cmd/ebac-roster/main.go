// ebac-roster is the roster plugin of ebac: it derives the figaro family's
// role registry from the aria list the core passes on stdin (aria-roster).
// It reaches nothing itself: no socket, no credential.
package main

import (
	"github.com/gluck/ebac/internal/plugin"
	"github.com/gluck/ebac/internal/rosterstate"
)

func main() { plugin.Serve(rosterstate.Plugin{}.Handle) }
