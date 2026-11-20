//go:build ebaclive

package teams

import (
	"os"
	"time"
)

// blindAfter, in a binary built with -tags ebaclive (core's live tests only),
// takes EBAC_TEAMS_BLIND_AFTER (a Go duration) so a test can shorten the
// threshold in the plugin process. The shipped binary never reads it (blind.go).
func blindAfter() time.Duration {
	if v := os.Getenv("EBAC_TEAMS_BLIND_AFTER"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return teamsBlindAfter
}
