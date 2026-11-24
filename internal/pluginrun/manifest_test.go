package pluginrun

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// store makes a fake store root with a plugin dir in it and returns the dir.
func store(t *testing.T) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	old := StoreRoot
	StoreRoot = root + "/"
	t.Cleanup(func() { StoreRoot = old })
	dir := filepath.Join(root, "plugins")
	if err := os.MkdirAll(filepath.Join(dir, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

// ship writes ebac-<name> and its manifest; mut edits the manifest first.
func ship(t *testing.T, dir, name string, sources []string, mut func(m map[string]any)) {
	t.Helper()
	body := []byte("#!/bin/sh\necho " + name + "\n")
	if err := os.WriteFile(filepath.Join(dir, "bin", "ebac-"+name), body, 0o755); err != nil {
		t.Fatal(err)
	}
	h := sha256.Sum256(body)
	m := map[string]any{"name": name, "exe": "bin/ebac-" + name, "sha256": hex.EncodeToString(h[:]),
		"protocol": map[string]int{"major": 1, "minor": 1}, "sources": sources}
	if mut != nil {
		mut(m)
	}
	raw, _ := json.Marshal(m)
	if err := os.WriteFile(filepath.Join(dir, name+".plugin.json"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestDiscoverAcceptsAPinnedPluginAndRoutesItsSources(t *testing.T) {
	dir := store(t)
	ship(t, dir, "tomb", []string{"teams-chat"}, nil)
	ship(t, dir, "teamslive", []string{"teams-live"}, nil)
	found, refused, err := Discover(dir)
	if err != nil || len(refused) != 0 {
		t.Fatalf("err %v, refused %v", err, refused)
	}
	if f, ok := found["tomb"]; !ok || f.Path != filepath.Join(dir, "bin", "ebac-tomb") {
		t.Fatalf("found = %+v", found)
	}
	if _, ok := found["teamslive"]; !ok {
		t.Fatal("teamslive not found")
	}
}

func TestDiscoverRefusesEachBadPluginAndKeepsTheOthers(t *testing.T) {
	dir := store(t)
	ship(t, dir, "tomb", []string{"teams-chat"}, nil)
	cases := map[string]struct {
		name    string
		sources []string
		mut     func(m map[string]any)
		why     string
	}{
		"sha":     {"gh", []string{"github-pr"}, func(m map[string]any) { m["sha256"] = strings.Repeat("0", 64) }, "manifest pins"},
		"major":   {"keel", []string{"keel-threads"}, func(m map[string]any) { m["protocol"] = map[string]int{"major": 2, "minor": 0} }, "speaks protocol 2.0, this ebac speaks 1.1"},
		"unknown": {"roster", []string{"aria-roster"}, func(m map[string]any) { m["grants"] = "all" }, "unknown field"},
		"escape":  {"bandhub", []string{"band-hub"}, func(m map[string]any) { m["exe"] = "../bin/ebac-bandhub" }, "inside the plugin dir"},
		"route":   {"approve", []string{"teams-chat-x"}, nil, "not one this ebac routes"},
	}
	for _, c := range cases {
		ship(t, dir, c.name, c.sources, c.mut)
	}
	found, refused, err := Discover(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := found["tomb"]; !ok {
		t.Fatal("the good plugin was not kept")
	}
	for k, c := range cases {
		key := c.name
		if k == "unknown" {
			key = c.name + ".plugin.json"
		}
		why, ok := refused[key]
		if !ok || !strings.Contains(why.Error(), c.why) {
			t.Errorf("%s: refused[%s] = %v, want %q", k, key, why, c.why)
		}
		if _, ok := found[c.name]; ok {
			t.Errorf("%s: %s was found", k, c.name)
		}
	}
}

func TestAnExeWithoutAManifestIsNeverFound(t *testing.T) {
	dir := store(t)
	if err := os.WriteFile(filepath.Join(dir, "bin", "ebac-gh"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	found, _, err := Discover(dir)
	if err != nil || len(found) != 0 {
		t.Fatalf("found %v, err %v", found, err)
	}
	r := &Runner{Dir: dir, Found: found}
	if _, err := r.exe("gh"); err == nil || !strings.Contains(err.Error(), "no manifest") {
		t.Fatalf("exe(gh) err = %v", err)
	}
}

func TestADirOutsideTheStoreIsRefused(t *testing.T) {
	store(t)
	if _, _, err := Discover(t.TempDir()); err == nil || !strings.Contains(err.Error(), "Nix store path") {
		t.Fatalf("err = %v", err)
	}
}

func TestTwoPluginsClaimingOneSourceAreBothRefused(t *testing.T) {
	dir := store(t)
	ship(t, dir, "tomb", []string{"teams-chat"}, nil)
	ship(t, dir, "teamslive", []string{"teams-live", "teams-chat"}, nil)
	found, refused, err := Discover(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"tomb", "teamslive"} {
		if _, ok := found[n]; ok {
			t.Errorf("%s found despite the shared source", n)
		}
		if why := refused[n]; why == nil || !strings.Contains(why.Error(), "claimed by") {
			t.Errorf("%s: refused = %v", n, why)
		}
	}
}
