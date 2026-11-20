module github.com/gluck/ebac

go 1.25.8

require (
	github.com/gluck/ebac/sdk v0.0.0
	github.com/jack-work/figaro v0.76.1
)

replace github.com/gluck/ebac/sdk => ./sdk

require (
	github.com/coder/websocket v1.8.14 // indirect
	github.com/jack-work/jkrpc v0.1.0 // indirect
)
