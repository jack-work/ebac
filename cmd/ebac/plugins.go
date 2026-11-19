package main

import (
	"os"
	"path/filepath"

	"github.com/gluck/ebac/internal/bandstate"
	"github.com/gluck/ebac/internal/figaro"
	"github.com/gluck/ebac/internal/identity"
	"github.com/gluck/ebac/internal/plugin"
	"github.com/gluck/ebac/internal/source/approve"
	"github.com/gluck/ebac/internal/source/keel"
)

// grants are this process's access grants, by the paths that carry them: the
// sandbox shows each plugin only the ones it needs (plugin.Needs).
func grants(g globals) plugin.Grants {
	gr := plugin.Grants{
		plugin.GrantFigaro:   {figaro.AngelusSocket()},
		plugin.GrantKeelDoor: {keel.Socket()},
		plugin.GrantBandHub:  {bandstate.DefaultSocket},
		// Hides /run/band-grants (request and honor sockets) from every
		// plugin but org-approve's.
		plugin.GrantBandGrants: {approve.RequestSocket},
		// Never in any plugin's Needs: hidden from every plugin.
		plugin.GrantStampKey: {identity.KeyPath()},
	}
	if g.figaro != "" {
		gr[plugin.GrantFigaro] = append(gr[plugin.GrantFigaro], g.figaro)
	}
	if rt := os.Getenv("XDG_RUNTIME_DIR"); rt != "" {
		gr[plugin.GrantFigaro] = append(gr[plugin.GrantFigaro], filepath.Join(rt, "figaro", "angelus.sock"))
		gr[plugin.GrantCredCache] = []string{filepath.Join(rt, "ebac-cred")}
	}
	if s := os.Getenv("EBAC_BAND_CRED_SOCKET"); s != "" {
		gr[plugin.GrantBandCred] = []string{s}
	}
	return gr
}

func newRunner(g globals) (*plugin.Runner, error) { return plugin.NewRunner(grants(g)) }
