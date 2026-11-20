module github.com/gluck/ebac/plugins/github

go 1.25.8

require (
	github.com/gluck/ebac v0.0.0
	github.com/gluck/ebac/sdk v0.0.0
)

require (
	github.com/coder/websocket v1.8.14 // indirect
	github.com/jack-work/figaro v0.76.1 // indirect
	github.com/jack-work/jkrpc v0.1.0 // indirect
)

// The core is required only by the EBAC_LIVE tests (they drive the plugin
// through the core's poller); the plugin binaries import sdk only.
replace (
	github.com/gluck/ebac => ../..
	github.com/gluck/ebac/sdk => ../../sdk
)
