package main

import "testing"

func TestChangesRoster(t *testing.T) {
	for _, c := range []struct {
		cmd  string
		args []string
		want bool
	}{
		{"add", []string{"--name", "x"}, true},
		{"add-teams", []string{"--name", "x"}, true},
		{"add-band-hub", nil, true},
		{"rm", []string{"--critic", "x"}, true},
		{"remove", []string{"--critic", "x"}, true},
		{"stop", []string{"--critic", "x"}, true},
		{"resume", []string{"--critic", "x"}, true},
		{"add-roster", nil, false}, // makes the roster itself
		{"add", []string{"--help"}, false},
		{"rm", []string{"--critic", "x", "--dry-run"}, false},
		{"ls", nil, false},
		{"poll", []string{"--critic", "x"}, false},
		{"set", []string{"--critic", "x"}, false},
	} {
		if got := changesRoster(c.cmd, c.args); got != c.want {
			t.Errorf("changesRoster(%s %v) = %v, want %v", c.cmd, c.args, got, c.want)
		}
	}
}
