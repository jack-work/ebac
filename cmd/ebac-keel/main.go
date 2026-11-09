// ebac-keel is the keel plugin of ebac: one keel repo's comment threads
// (keel-threads). It speaks internal/plugin on stdin/stdout.
package main

import (
	"github.com/gluck/ebac/internal/plugin"
	"github.com/gluck/ebac/internal/source/keel"
)

func main() { plugin.Serve(keel.Plugin{}.Handle) }
