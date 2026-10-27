package author

import "github.com/gluck/ebac/internal/poll"

// The binary registers every source at start-up; a test that runs a whole
// round registers the one it exercises.
func init() { poll.Register(Source{}) }
