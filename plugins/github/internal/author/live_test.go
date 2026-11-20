package author

import (
	"os"
	"testing"

	"github.com/gluck/ebac/internal/figaro"
)

// Live tests run against the real angelus. They are gated because they mint
// and kill real arias; run with:
//
//	EBAC_LIVE=1 go test -run Live ./...
//
// They exist because the two things the SDK conversion changed most are the
// two that cannot be unit-tested: whether a role-targeted send still reaches
// the holder, and whether a conditional write actually refuses a stale base.
// Both were previously "should work" and both are load-bearing.
func liveOrSkip(t *testing.T) *figaro.Figaro {
	t.Helper()
	if os.Getenv("EBAC_LIVE") != "1" {
		t.Skip("set EBAC_LIVE=1 to run against the live daemon")
	}
	f := figaro.NewFigaro("")
	t.Cleanup(func() { _ = f.Close() })
	return f
}
