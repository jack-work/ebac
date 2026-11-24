package model

import "testing"

func TestGHCacheWakesOnWriteFieldsAndTheirAncestorsOnly(t *testing.T) {
	for path, want := range map[string]bool{
		"gh.title": true, "gh.body": true, "gh.labels": true, "gh.draft": true,
		"gh.threads.00000001.resolved": true, "gh.threads.00000001.replies": true,
		"gh.threads.00000001.comments.00000002.body": true, "gh.comments.00000003.body": true,
		"want.comments.new.k1": true, "want.comments.new.k1.body": true, "ours.praxis_new.000000000001": true,
		// ancestors: a whole subtree written at once may hold a write field
		"gh": true, "gh.threads": true, "gh.comments.00000003": true, "want": true, "ours": true,
		// read-only and unknown keys never wake
		"gh.state": false, "gh.head": false, "gh.comments.00000003.author": false, "gh.threads.00000001.path": false,
		"sync": false, "sync.alarm.conflict.labels": false, "want.title": false, "want.create": false,
		"ours.other": false, "track": false, "": false, "gh.titles": false,
	} {
		if got := GHCacheWakes(path); got != want {
			t.Errorf("GHCacheWakes(%q) = %v, want %v", path, got, want)
		}
	}
}
