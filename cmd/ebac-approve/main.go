// ebac-approve is the approve-requests plugin of ebac: a sandboxed principal's
// APPROVE requests on bic/aether, checked and posted with ebac's credential.
// It speaks internal/plugin on stdin/stdout. Its only GHE write is an APPROVE
// review (internal/source/approve.GH).
package main

import (
	"github.com/gluck/ebac/internal/plugin"
	"github.com/gluck/ebac/internal/source/approve"
)

func main() { plugin.Serve(approve.Plugin{}.Handle) }
