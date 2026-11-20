module github.com/gluck/ebac/plugins/keel

go 1.25.8

require github.com/gluck/ebac/sdk v0.0.0

require github.com/jack-work/figaro v0.76.1

require (
	github.com/coder/websocket v1.8.14 // indirect
	github.com/jack-work/jkrpc v0.1.0 // indirect
)

// The core module is a TEST-ONLY dependency: the EBAC_LIVE tests drive this
// plugin through the core poller against a live daemon. The plugin binary
// links only the sdk.
require github.com/gluck/ebac v0.0.0

replace (
	github.com/gluck/ebac => ../..
	github.com/gluck/ebac/sdk => ../../sdk
)
