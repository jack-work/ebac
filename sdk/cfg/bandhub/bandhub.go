// Package bandhub holds the band-hub plugin's configuration that the core CLI
// also needs (defaults for add-band-hub). The plugin itself lives in
// plugins/bandhub; the core never imports it.
package bandhub

// DefaultRepo is where the hub's branch-carrying jobs (aether.*) build from.
const DefaultRepo = "bic/aether"
