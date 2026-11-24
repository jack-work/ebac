package pluginrun

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/gluck/ebac/sdk/plugin"
)

// StoreRoot is where a plugin directory and every plugin exe must resolve to.
// The Nix store is written only by a build, so a seat cannot drop a binary in
// or swap one between the sha check and the exec. Tests point it at a temp dir.
var StoreRoot = "/nix/store/"

// Manifest is <name>.plugin.json in the plugin directory: what a plugin
// package declares about the one exe it ships (built by ebac's flake helper
// lib.ebacPlugin, which records the sha256 at build time).
type Manifest struct {
	Name     string   `json:"name"`
	Exe      string   `json:"exe"`
	SHA256   string   `json:"sha256"`
	Protocol Version  `json:"protocol"`
	Sources  []string `json:"sources"`
}

// Version is a plugin protocol version, major.minor.
type Version struct {
	Major int `json:"major"`
	Minor int `json:"minor"`
}

func (v Version) String() string { return fmt.Sprintf("%d.%d", v.Major, v.Minor) }

// Found is one plugin the discovery accepted: its exe, resolved into the store.
type Found struct {
	Manifest
	Path string
}

var (
	nameRe = regexp.MustCompile(`^[a-z][a-z0-9-]{0,30}$`)
	shaRe  = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// Discover reads every *.plugin.json in dir. A plugin is accepted only when its
// manifest is well formed, its exe resolves into the store and hashes to the
// manifest's sha256, its protocol major is this core's and no other manifest
// claims one of its sources; anything else refuses that plugin (the reason is
// kept in refused, by name or file) and the others stand. An exe without a
// manifest is never found.
func Discover(dir string) (found map[string]Found, refused map[string]error, err error) {
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return nil, nil, fmt.Errorf("plugin dir %s: %w", dir, err)
	}
	if !inStore(real) {
		return nil, nil, fmt.Errorf("plugin dir %s resolves to %s, outside %s: refused (EBAC_PLUGIN_DIR must be a Nix store path)", dir, real, StoreRoot)
	}
	files, err := filepath.Glob(filepath.Join(dir, "*.plugin.json"))
	if err != nil {
		return nil, nil, err
	}
	sort.Strings(files)
	found, refused = map[string]Found{}, map[string]error{}
	claims := map[string][]string{}
	for _, f := range files {
		m, path, err := load(dir, f)
		if err != nil {
			refused[filepath.Base(f)] = err
			continue
		}
		if _, dup := found[m.Name]; dup {
			refused[m.Name] = fmt.Errorf("plugin %s: two manifests name it", m.Name)
			delete(found, m.Name)
			continue
		}
		found[m.Name] = Found{Manifest: m, Path: path}
		for _, s := range m.Sources {
			claims[s] = append(claims[s], m.Name)
		}
	}
	for src, names := range claims {
		if len(names) < 2 {
			continue
		}
		for _, n := range names {
			refused[n] = fmt.Errorf("plugin %s: source %s is claimed by %s", n, src, strings.Join(names, " and "))
			delete(found, n)
		}
	}
	for name, f := range found {
		for _, s := range f.Sources {
			if want, err := For(s); err != nil || want != name {
				refused[name] = fmt.Errorf("plugin %s: source %s is not one this ebac routes to it (%v)", name, s, orName(want, err))
				delete(found, name)
				break
			}
		}
	}
	return found, refused, nil
}

func orName(name string, err error) string {
	if err != nil {
		return err.Error()
	}
	return "it routes to " + name
}

func load(dir, file string) (Manifest, string, error) {
	var m Manifest
	raw, err := os.ReadFile(file)
	if err != nil {
		return m, "", fmt.Errorf("manifest %s: %w", file, err)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return m, "", fmt.Errorf("manifest %s: %w", filepath.Base(file), err)
	}
	switch {
	case !nameRe.MatchString(m.Name):
		return m, "", fmt.Errorf("manifest %s: name %q is not a plugin name", filepath.Base(file), m.Name)
	case filepath.Base(file) != m.Name+".plugin.json":
		return m, "", fmt.Errorf("manifest %s names plugin %s", filepath.Base(file), m.Name)
	case m.Exe == "" || filepath.IsAbs(m.Exe) || strings.Contains(m.Exe, ".."):
		return m, "", fmt.Errorf("plugin %s: exe %q must be a path inside the plugin dir", m.Name, m.Exe)
	case filepath.Base(m.Exe) != "ebac-"+m.Name:
		return m, "", fmt.Errorf("plugin %s: exe %q is not ebac-%s", m.Name, m.Exe, m.Name)
	case !shaRe.MatchString(m.SHA256):
		return m, "", fmt.Errorf("plugin %s: sha256 %q is not 64 lowercase hex", m.Name, m.SHA256)
	case m.Protocol.Major != plugin.Version:
		return m, "", fmt.Errorf("plugin %s speaks protocol %s, this ebac speaks %d.%d", m.Name, m.Protocol, plugin.Version, plugin.Minor)
	case m.Protocol.Minor > plugin.Minor:
		return m, "", fmt.Errorf("plugin %s speaks protocol %s, newer than this ebac's %d.%d", m.Name, m.Protocol, plugin.Version, plugin.Minor)
	case len(m.Sources) == 0:
		return m, "", fmt.Errorf("plugin %s: serves no source", m.Name)
	}
	path, err := filepath.EvalSymlinks(filepath.Join(dir, m.Exe))
	if err != nil {
		return m, "", fmt.Errorf("plugin %s: exe: %w", m.Name, err)
	}
	if !inStore(path) {
		return m, "", fmt.Errorf("plugin %s: exe resolves to %s, outside %s", m.Name, path, StoreRoot)
	}
	got, err := sum(path)
	if err != nil {
		return m, "", fmt.Errorf("plugin %s: %w", m.Name, err)
	}
	if got != m.SHA256 {
		return m, "", fmt.Errorf("plugin %s: exe sha256 %s, manifest pins %s", m.Name, got, m.SHA256)
	}
	return m, path, nil
}

func inStore(path string) bool {
	return strings.HasPrefix(filepath.Clean(path)+"/", StoreRoot)
}

func sum(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
