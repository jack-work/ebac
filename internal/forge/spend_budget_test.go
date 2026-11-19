package forge

import (
	"encoding/json"
	"testing"
	"time"
)

// The spend a round reports to the core: GraphQL points with the account's remaining and reset, REST calls as all
// calls minus GraphQL reads; a round that asked nothing reports nothing. RED if a limit name, a count or the
// remaining/reset pass-through changes.
func TestSpendBudgetWireBytes(t *testing.T) {
	reset := time.Date(2026, 11, 19, 7, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		name string
		s    Spend
		want string
	}{
		{"graphql and rest", Spend{All: 5, Calls: 2, Points: 7, Remaining: 4321, ResetAt: reset},
			`[{"limit":"graphql","n":7,"remaining":4321,"reset_at":"2026-11-19T07:00:00Z"},{"limit":"rest","n":3,"remaining":-1}]`},
		{"rest only", Spend{All: 2, Calls: 0, Remaining: -1}, `[{"limit":"rest","n":2,"remaining":-1}]`},
		{"graphql only", Spend{All: 1, Calls: 1, Points: 1, Remaining: 900}, `[{"limit":"graphql","n":1,"remaining":900}]`},
		{"nothing asked", Spend{Remaining: -1}, `null`},
	} {
		b, _ := json.Marshal(c.s.Budget())
		if string(b) != c.want {
			t.Errorf("%s:\n got %s\nwant %s", c.name, b, c.want)
		}
	}
}

// The rate-limit selection asks the reset too (the floor waits for it); an existing rateLimit is left alone.
func TestWithRateLimitAsksTheReset(t *testing.T) {
	if got := withRateLimit("query{ a: repository(owner:\"o\",name:\"r\"){id} }"); got != "query{ rateLimit{cost remaining resetAt}  a: repository(owner:\"o\",name:\"r\"){id} }" {
		t.Fatalf("got %q", got)
	}
	if got := withRateLimit("query{ rateLimit{cost} x }"); got != "query{ rateLimit{cost} x }" {
		t.Fatalf("an existing rateLimit was touched: %q", got)
	}
}
