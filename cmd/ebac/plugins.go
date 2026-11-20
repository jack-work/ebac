package main

import (
	"github.com/gluck/ebac/internal/pluginrun"
	"github.com/gluck/ebac/sdk/bandhub"
	"os"
	"path/filepath"

	"github.com/gluck/ebac/internal/figaro"
	"github.com/gluck/ebac/internal/identity"
	"github.com/gluck/ebac/internal/source/approve"
	"github.com/gluck/ebac/internal/source/keel"
)

// grants are this process's access grants, by the paths that carry them: the
// sandbox shows each plugin only the ones it needs (pluginrun.Needs).
func grants(g globals) pluginrun.Grants {
	gr := pluginrun.Grants{
		pluginrun.GrantFigaro:   {figaro.AngelusSocket()},
		pluginrun.GrantKeelDoor: {keel.Socket()},
		pluginrun.GrantBandHub:  {bandhub.DefaultSocket},
		// Hides /run/band-grants (request and honor sockets) from every
		// plugin but org-approve's.
		pluginrun.GrantBandGrants: {approve.RequestSocket},
		// Never in any plugin's Needs: hidden from every plugin.
		pluginrun.GrantStampKey: {identity.KeyPath()},
	}
	if g.figaro != "" {
		gr[pluginrun.GrantFigaro] = append(gr[pluginrun.GrantFigaro], g.figaro)
	}
	if rt := os.Getenv("XDG_RUNTIME_DIR"); rt != "" {
		gr[pluginrun.GrantFigaro] = append(gr[pluginrun.GrantFigaro], filepath.Join(rt, "figaro", "angelus.sock"))
		gr[pluginrun.GrantCredCache] = []string{filepath.Join(rt, "ebac-cred")}
	}
	if s := os.Getenv("EBAC_BAND_CRED_SOCKET"); s != "" {
		gr[pluginrun.GrantBandCred] = []string{s}
	}
	return gr
}

func newRunner(g globals) (*pluginrun.Runner, error) { return pluginrun.NewRunner(grants(g)) }
