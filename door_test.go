package main

import (
	"testing"
)

// figaro 0.48 stopped returning a per-aria endpoint from Attach/Create; an
// empty endpoint dialed as "unsupported transport scheme \"\"" and every form
// write failed. Every aria and form must be reached through the daemon's door.
func TestAriaAddrIsTheDaemonDoor(t *testing.T) {
	f := NewFigaro("/run/user/1000/figaro/angelus.sock")
	a := f.addr("ae9d5dc47e2616dcae931e47148d1172")
	if a.Door.Scheme != "unix" || a.Door.Address != "/run/user/1000/figaro/angelus.sock" {
		t.Fatalf("door = %+v; want unix:/run/user/1000/figaro/angelus.sock", a.Door)
	}
	if a.ID != "ae9d5dc47e2616dcae931e47148d1172" {
		t.Fatalf("id = %q", a.ID)
	}
}
