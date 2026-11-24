package model

import "strings"

// GHCacheWakePaths are the entity-form paths whose change by a seat asks
// for a write (augustine contract 5, A3 write-on-change): the cache's write
// fields and its creation slots. ebacd follows every entity form of a
// github-cache critic and, on a change touching one of these (or an
// ancestor of one, a whole subtree written at once), runs that entity's
// write within seconds instead of at the next cadence round. "*" is one
// path segment. The plugin's three-way base, not this list, decides whether
// anything is written: a change by ebac itself costs one local check.
var GHCacheWakePaths = []string{
	"gh.title", "gh.body", "gh.labels", "gh.draft",
	"gh.threads.*.resolved", "gh.threads.*.replies", "gh.threads.*.comments.*.body",
	"gh.comments.*.body",
	"want.comments.new.*",
	"ours.praxis_new.*",
}

// GHCacheWakes reports whether a change at path (dot-separated) can touch a
// write field: path is one of GHCacheWakePaths, lies under one, or is an
// ancestor of one.
func GHCacheWakes(path string) bool {
	if path == "" {
		return false
	}
	got := strings.Split(path, ".")
	for _, pat := range GHCacheWakePaths {
		want := strings.Split(pat, ".")
		n := min(len(got), len(want))
		match := true
		for i := 0; i < n; i++ {
			if want[i] != "*" && want[i] != got[i] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}
