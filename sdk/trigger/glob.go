package trigger

import (
	"fmt"
	"path"
	"strings"
)

// Glob reports whether p matches g. Segments are separated by '/'; a segment
// "**" matches any number of whole segments (zero included); any other
// segment is a path.Match pattern for exactly one segment. An empty p never
// matches: an event with no path is not on any file.
func Glob(g, p string) bool {
	if p == "" {
		return false
	}
	return globSegs(strings.Split(g, "/"), strings.Split(p, "/"))
}

func globSegs(gs, ps []string) bool {
	for len(gs) > 0 {
		if gs[0] == "**" {
			for len(gs) > 0 && gs[0] == "**" {
				gs = gs[1:]
			}
			if len(gs) == 0 {
				return true
			}
			for i := 0; i <= len(ps); i++ {
				if globSegs(gs, ps[i:]) {
					return true
				}
			}
			return false
		}
		if len(ps) == 0 {
			return false
		}
		if ok, _ := path.Match(gs[0], ps[0]); !ok {
			return false
		}
		gs, ps = gs[1:], ps[1:]
	}
	return len(ps) == 0
}

// CheckGlob refuses a pattern Glob could not apply: empty, a bad
// path.Match segment, or "**" glued to other text in one segment.
func CheckGlob(g string) error {
	if strings.TrimSpace(g) == "" {
		return fmt.Errorf("empty glob")
	}
	for _, seg := range strings.Split(g, "/") {
		if seg == "**" {
			continue
		}
		if strings.Contains(seg, "**") {
			return fmt.Errorf("segment %q: '**' must be a whole segment", seg)
		}
		if _, err := path.Match(seg, ""); err != nil {
			return fmt.Errorf("segment %q: %w", seg, err)
		}
	}
	return nil
}
