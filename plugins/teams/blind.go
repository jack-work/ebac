//go:build !ebaclive

package teams

import "time"

// blindAfter is the shipped threshold: no environment can change it.
func blindAfter() time.Duration { return teamsBlindAfter }
