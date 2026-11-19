package identity

import (
	"fmt"
	"strings"
	"testing"
)

func id(n int) string  { return fmt.Sprintf("%032x", n) }
func fid(n int) string { return fmt.Sprintf("@%08x%024x", n, 0) }

// holder 1 holds reviewer; 2 is its fork, 3 a fork of 2, 4 of 3; 9 holds
// knight; 10 is a root; 11's parent is a form; 12's parent (55) is unlisted;
// 56 is reaped itself; 57 is a REAPED fork of 1 and 58 a live fork of 57.
func dir() Directory {
	return Directory{
		Forms: map[string][]string{"reviewer": {fid(801)}, "knight": {fid(601)}, "empty": {fid(1)},
			"dup": {fid(902), fid(901)}},
		Holder: map[string]string{fid(801): id(1), fid(601): id(9), fid(1): "", fid(901): id(1), fid(902): id(1)},
		Parent: map[string]string{id(1): "", id(2): id(1), id(3): id(2), id(4): id(3), id(9): "", id(10): "",
			id(11): fid(801), id(12): id(55), id(56): "", id(57): id(1), id(58): id(57)},
		Reaped: map[string]bool{id(56): true, id(57): true},
	}
}

func TestMayPostAndMayVerdictArms(t *testing.T) {
	d := dir()
	type res struct {
		ok  bool
		why string
	}
	cases := []struct {
		name, caller, role string
		post, verdict      res
	}{
		{"the holder", id(1), "reviewer", res{true, ""}, res{true, ""}},
		{"a fork", id(2), "reviewer", res{true, ""}, res{false, "is not the holder of role reviewer"}},
		{"three forks down", id(4), "reviewer", res{true, ""}, res{false, "is not the holder"}},
		{"another role's holder", id(9), "reviewer", res{false, "is not a live fork of its holder"}, res{false, "is not the holder"}},
		{"parent is a form", id(11), "reviewer", res{false, "is not a live fork"}, res{false, "is not the holder"}},
		{"parent unlisted", id(12), "reviewer", res{false, "cannot be established"}, res{false, "is not the holder"}},
		{"a fork through a reaped ancestor", id(58), "reviewer", res{true, ""}, res{false, "is not the holder"}},
		{"the reaped ancestor itself", id(57), "reviewer", res{false, "is reaped"}, res{false, "is reaped"}},
		{"caller reaped", id(56), "reviewer", res{false, "is reaped"}, res{false, "is reaped"}},
		{"unknown role", id(1), "warden", res{false, `role "warden": no live role form of that name`}, res{false, "no live role form"}},
		{"duplicate name", id(1), "dup", res{false, "2 live forms carry that name (" + fid(901) + ", " + fid(902) + "): ambiguous, refused"}, res{false, "ambiguous"}},
		{"no seat", id(1), "empty", res{false, "has no seat cast"}, res{false, "has no seat cast"}},
		{"caller unlisted", id(77), "reviewer", res{false, "is not a live aria"}, res{false, "is not a live aria"}},
		{"short caller", "e680885d", "reviewer", res{false, "want a full 32-hex aria id"}, res{false, "32-hex"}},
	}
	for _, c := range cases {
		for kind, want := range map[string]res{"post": c.post, "verdict": c.verdict} {
			f := d.MayPost
			if kind == "verdict" {
				f = d.MayVerdict
			}
			_, ok, why := f(c.caller, c.role)
			if ok != want.ok || (want.why == "" && why != "") || !strings.Contains(why, want.why) {
				t.Errorf("%s %s: ok %v why %q, want %v %q", c.name, kind, ok, why, want.ok, want.why)
			}
		}
	}
}

func TestMayPostRefusesLoopsAndLongChains(t *testing.T) {
	d := dir()
	d.Parent[id(20)], d.Parent[id(21)] = id(21), id(20)
	if _, ok, why := d.MayPost(id(20), "reviewer"); ok || !strings.Contains(why, "cannot be established") {
		t.Errorf("loop: %v %q", ok, why)
	}
	prev := id(1)
	for i := 0; i <= MaxHops; i++ {
		n := id(100 + i)
		d.Parent[n] = prev
		prev = n
	}
	if _, ok, why := d.MayPost(id(100+MaxHops-1), "reviewer"); !ok {
		t.Errorf("%d forks down refused: %s", MaxHops, why)
	}
	if _, ok, why := d.MayPost(id(100+MaxHops), "reviewer"); ok || !strings.Contains(why, "cannot be established") {
		t.Errorf("%d forks down: %v %q", MaxHops+1, ok, why)
	}
}

// The author-side check fails CLOSED: past the bound, through a reaped link
// or a loop, the caller is not independent.
func TestIndependentFailsClosed(t *testing.T) {
	d := dir()
	prev := id(9)
	for i := 0; i <= MaxHops; i++ {
		n := id(300 + i)
		d.Parent[n] = prev
		prev = n
	}
	d.Parent[id(20)], d.Parent[id(21)] = id(21), id(20)
	cases := []struct {
		name, caller, author string
		ok                   bool
		why                  string
	}{
		{"unrelated root", id(1), id(9), true, ""},
		{"the author", id(9), id(9), false, "is the author side"},
		{"a fork of the author", id(300), id(9), false, "is the author side"},
		{"33 forks below the author", id(300 + MaxHops), id(9), false, "cannot be established"},
		{"unlisted link", id(12), id(9), false, "cannot be established"},
		{"a fork of the author through a reaped ancestor", id(58), id(1), false, "is the author side"},
		{"loop", id(20), id(9), false, "cannot be established"},
		{"no author holder", id(1), "", false, "has no holder"},
	}
	for _, c := range cases {
		ok, why := d.Independent(c.caller, c.author)
		if ok != c.ok || (c.why == "" && why != "") || !strings.Contains(why, c.why) {
			t.Errorf("%s: %v %q, want %v %q", c.name, ok, why, c.ok, c.why)
		}
	}
}

// The bound is far above live depth (29 measured): a 64-fork chain still
// counts; pinned with a literal, not relative to MaxHops.
func TestMaxHopsAdmitsA64ForkChain(t *testing.T) {
	d := dir()
	prev := id(1)
	for i := 0; i < 64; i++ {
		n := id(500 + i)
		d.Parent[n] = prev
		prev = n
	}
	if _, ok, why := d.MayPost(id(500+63), "reviewer"); !ok {
		t.Fatalf("64 forks down refused: %s", why)
	}
	if ok, why := d.Independent(id(500+63), id(9)); !ok {
		t.Fatalf("64 forks below an unrelated root not independent: %s", why)
	}
}
