package plugin

import (
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/gluck/ebac/internal/model"
)

// probe is the fake plugin's look at the sandbox from inside: it reaches for
// every path in EBAC_FAKE_PROBE (dial a socket, read a file), then tries to
// undo the sandbox two ways. Every outcome is reported as text.
func probe() []string {
	res := func(err error) string {
		if err == nil {
			return "ok"
		}
		return err.Error()
	}
	var out []string
	for _, p := range strings.Split(os.Getenv("EBAC_FAKE_PROBE"), "\n") {
		if p == "" {
			continue
		}
		var err error
		if strings.HasSuffix(p, ".sock") {
			var c net.Conn
			if c, err = net.Dial("unix", p); err == nil {
				c.Close()
			}
		} else {
			_, err = os.ReadFile(p)
		}
		out = append(out, p+": "+res(err))
	}
	if d := os.Getenv("EBAC_FAKE_UMOUNT"); d != "" {
		out = append(out, "umount: "+res(syscall.Unmount(d, syscall.MNT_DETACH)))
	}
	if u := os.Getenv("EBAC_FAKE_UNSHARE"); u != "" {
		b, err := exec.Command(u, "--user", "--map-root-user", "true").CombinedOutput()
		msg := res(err)
		if err != nil {
			msg += ": " + strings.TrimSpace(string(b))
		}
		out = append(out, "userns: "+msg)
	}
	return out
}

// testBwrap is the bubblewrap these tests run: the build's own, else
// EBAC_TEST_BWRAP. A bwrap that cannot make a user namespace here (a build
// sandbox) skips with its error text rather than failing or passing blind.
func testBwrap(t *testing.T) string {
	t.Helper()
	bw := BwrapPath
	if bw == "" {
		bw = os.Getenv("EBAC_TEST_BWRAP")
	}
	if bw == "" {
		t.Skip("no bubblewrap: build with plugin.BwrapPath or set EBAC_TEST_BWRAP")
	}
	cmd := exec.Command(bw, (&Sandbox{Bwrap: bw}).Args(os.Args[0], nil)...)
	cmd.Args = append(cmd.Args, "-test.run=^$")
	cmd.Env = []string{"PATH=" + os.Getenv("PATH")}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("bubblewrap cannot sandbox here: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return bw
}

// box is a stand-in for the core's grants: one live socket (or token file)
// per grant, each in its own directory as on the box.
type box struct {
	grants Grants
	probe  map[Grant]string // the path the probe reaches for, per grant
}

func newBox(t *testing.T) *box {
	t.Helper()
	root := t.TempDir()
	b := &box{grants: Grants{}, probe: map[Grant]string{}}
	sock := func(g Grant, dir string) {
		d := filepath.Join(root, dir)
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
		p := filepath.Join(d, "x.sock")
		l, err := net.Listen("unix", p)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { l.Close() })
		go func() {
			for {
				c, err := l.Accept()
				if err != nil {
					return
				}
				c.Close()
			}
		}()
		b.grants[g], b.probe[g] = []string{p}, p
	}
	sock(GrantFigaro, "figaro")
	sock(GrantBandCred, "band-cred")
	sock(GrantKeelDoor, "kweb-ebac")
	sock(GrantBandHub, "band-hub")
	cache := filepath.Join(root, "ebac-cred")
	if err := os.Mkdir(cache, 0o700); err != nil {
		t.Fatal(err)
	}
	tok := filepath.Join(cache, "ghe-ebac.json")
	if err := os.WriteFile(tok, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	b.grants[GrantCredCache], b.probe[GrantCredCache] = []string{cache}, tok
	var paths []string
	for _, p := range b.probe {
		paths = append(paths, p)
	}
	t.Setenv("EBAC_FAKE_PROBE", strings.Join(paths, "\n"))
	t.Setenv("EBAC_FAKE_UMOUNT", filepath.Dir(b.probe[GrantFigaro]))
	if u, err := exec.LookPath("unshare"); err == nil {
		t.Setenv("EBAC_FAKE_UNSHARE", u)
	}
	return b
}

func lineFor(log []string, path string) string {
	for _, l := range log {
		if strings.HasPrefix(l, path+": ") {
			return strings.TrimPrefix(l, path+": ")
		}
	}
	return "(no line for " + path + ")"
}

// Each plugin's sandbox shows it exactly the grants it needs. Per plugin, the
// positive control is that its real dependencies answer from inside; the
// figaro socket and every grant it does not need are gone. Two further
// controls make a "gone" mean the sandbox: the core reaches every grant, and
// so does the same probe unsandboxed.
func TestSandboxShowsEachPluginOnlyItsGrants(t *testing.T) {
	bw := testBwrap(t)
	b := newBox(t)

	// Control 1: the core reaches every grant.
	for g, p := range b.probe {
		if strings.HasSuffix(p, ".sock") {
			c, err := net.Dial("unix", p)
			if err != nil {
				t.Fatalf("control: the core cannot reach %s at %s: %v", g, p, err)
			}
			c.Close()
		}
	}

	r := runner(t, "probe")
	critic := &model.Critic{Name: "c"}

	// Control 2: unsandboxed, the probe reaches every grant.
	open, err := r.Call(context.Background(), "fake", Request{Op: OpRound, Critic: *critic})
	if err != nil {
		t.Fatal(err)
	}
	for g, p := range b.probe {
		if got := lineFor(open.Log, p); got != "ok" {
			t.Fatalf("control: unsandboxed probe cannot reach %s (%s): %s; it proves nothing", g, p, got)
		}
	}
	userns := ""
	for _, l := range open.Log {
		if strings.HasPrefix(l, "userns: ") {
			userns = l
		}
	}

	for _, plug := range []string{"gh", "tomb", "keel", "bandhub", "roster"} {
		t.Run(plug, func(t *testing.T) {
			sb := &Sandbox{Bwrap: bw, Grants: b.grants}
			rr := *r
			rr.Sandbox = sb
			// command picks the profile by plugin name; serve the fake under
			// each real plugin name.
			if err := os.Symlink(filepath.Join(r.Dir, "ebac-fake"), filepath.Join(r.Dir, "ebac-"+plug)); err != nil && !os.IsExist(err) {
				t.Fatal(err)
			}
			got, err := rr.Call(context.Background(), plug, Request{Op: OpRound, Critic: *critic})
			if err != nil {
				t.Fatalf("sandboxed plugin failed outright: %v", err)
			}
			t.Logf("%s sees:\n%s", plug, strings.Join(got.Log, "\n"))
			need := map[Grant]bool{}
			for _, g := range Needs(plug, critic) {
				need[g] = true
			}
			for g, p := range b.probe {
				line := lineFor(got.Log, p)
				switch {
				case need[g] && line != "ok":
					t.Errorf("%s needs %s but cannot reach it: %s", plug, g, line)
				case !need[g] && !strings.HasSuffix(line, "no such file or directory"):
					t.Errorf("%s does not need %s, yet: %s", plug, g, line)
				}
			}
			if line := lineFor(got.Log, b.probe[GrantFigaro]); !strings.Contains(line, "connect: no such file or directory") {
				t.Errorf("%s reached figaro: %s", plug, line)
			}
			for _, l := range got.Log {
				if strings.HasPrefix(l, "umount: ") && l != "umount: operation not permitted" {
					t.Errorf("%s could unmount the tmpfs over figaro: %s", plug, l)
				}
				if strings.HasPrefix(l, "userns: ") && userns == "userns: ok" && !strings.Contains(l, "No space left on device") {
					t.Errorf("%s could create a nested user namespace (unsandboxed: %s): %s", plug, userns, l)
				}
			}
		})
	}
}

// A band-hub critic's own hub socket is reachable even when it is not the
// default one; the default stays hidden from the other plugins.
func TestSandboxHideHonoursACriticsHubSocket(t *testing.T) {
	b := newBox(t)
	sb := &Sandbox{Bwrap: "bwrap", Grants: b.grants}
	hub := b.probe[GrantBandHub]
	c := &model.Critic{Band: &model.BandRef{Socket: hub}}
	for _, d := range sb.Hide("bandhub", c) {
		if d == filepath.Dir(hub) {
			t.Fatalf("bandhub's own hub socket is hidden: %v", sb.Hide("bandhub", c))
		}
	}
	found := false
	for _, d := range sb.Hide("gh", c) {
		found = found || d == filepath.Dir(hub)
	}
	if !found {
		t.Fatalf("gh sees the hub: %v", sb.Hide("gh", c))
	}
}

// The figaro directory is hidden even if a needed grant lives in it.
func TestSandboxAlwaysHidesFigaro(t *testing.T) {
	b := newBox(t)
	fig := filepath.Dir(b.probe[GrantFigaro])
	b.grants[GrantBandCred] = []string{b.probe[GrantFigaro]}
	sb := &Sandbox{Bwrap: "bwrap", Grants: b.grants}
	for _, d := range sb.Hide("gh", &model.Critic{}) {
		if d == fig {
			return
		}
	}
	t.Fatalf("figaro not hidden when a needed grant shares its directory: %v", sb.Hide("gh", &model.Critic{}))
}

func TestNewSandboxFailsClosed(t *testing.T) {
	saved := BwrapPath
	t.Cleanup(func() { BwrapPath = saved })
	BwrapPath = ""
	t.Setenv(SandboxOffEnv, "")
	if _, err := NewSandbox(Grants{GrantFigaro: {"/x"}}); err == nil || !strings.Contains(err.Error(), "refusing to run plugins unsandboxed") {
		t.Fatalf("err = %v", err)
	}
	BwrapPath = "/nix/store/x/bin/bwrap"
	if _, err := NewSandbox(Grants{}); err == nil || !strings.Contains(err.Error(), "no figaro socket to hide") {
		t.Fatalf("err = %v", err)
	}
	t.Setenv(SandboxOffEnv, "off")
	if sb, err := NewSandbox(nil); sb != nil || err != nil {
		t.Fatalf("explicit off: %v %v", sb, err)
	}
}
