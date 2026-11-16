package main

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/gluck/ebac/internal/model"
)

func TestOverlayScope(t *testing.T) {
	cur := map[string]json.RawMessage{model.KeelThreadsScopeKey: json.RawMessage(`"changed"`), model.KeelMirrorKey: json.RawMessage(`["inbox"]`)}

	set, unset, spec, err := overlayScope(cur, map[string]bool{"inbox": true}, map[string]string{"inbox": "last:3"})
	if err != nil || !reflect.DeepEqual(set, map[string]json.RawMessage{model.KeelInboxScopeKey: json.RawMessage(`"last:3"`)}) || len(unset) != 0 {
		t.Fatalf("set inbox: %s %v %v", set, unset, err)
	}
	if spec.Threads != "changed" || spec.Inbox != "last:3" || strings.Join(spec.Mirror, ",") != "inbox" {
		t.Fatalf("scope after: %+v", spec)
	}

	set, unset, spec, err = overlayScope(cur, map[string]bool{"mirror": true, "threads": true}, map[string]string{"mirror": "", "threads": "all"})
	if err != nil || string(set[model.KeelThreadsScopeKey]) != `"all"` || len(set) != 1 || !reflect.DeepEqual(unset, []string{model.KeelMirrorKey}) || spec.Mirror != nil {
		t.Fatalf("clear mirror: %s %v %+v %v", set, unset, spec, err)
	}

	if _, _, _, err := overlayScope(cur, map[string]bool{"inbox": true}, map[string]string{"inbox": "last:0"}); err == nil || !strings.Contains(err.Error(), "last:N") {
		t.Fatalf("a bad inbox scope was accepted: %v", err)
	}
	broken := map[string]json.RawMessage{model.KeelThreadsScopeKey: json.RawMessage(`"recent"`)}
	if _, _, _, err := overlayScope(broken, map[string]bool{"inbox": true}, map[string]string{"inbox": "heads"}); err == nil || !strings.Contains(err.Error(), "recent") {
		t.Fatalf("a set over a broken key passed: %v", err)
	}
}

// A bad scope flag is refused before anything is minted.
func TestAddKeelRefusesABadScope(t *testing.T) {
	for _, flag := range [][]string{{"--threads", "recent"}, {"--inbox", "last:0"}, {"--mirror", "inbox.kind"}} {
		args := append([]string{"--name", "scopetest", "--repo", "jack/notes", "--waiting-on", "jack"}, flag...)
		err := cmdAddKeel(context.Background(), globals{}, args)
		if err == nil || !strings.Contains(err.Error(), strings.TrimPrefix(flag[1], "--")) {
			t.Fatalf("%v: %v", flag, err)
		}
	}
}

func TestSetWithNothingToSetNamesTheScopeFlags(t *testing.T) {
	err := cmdSet(context.Background(), globals{}, []string{"--critic", "x"})
	if err == nil || !strings.Contains(err.Error(), "--threads") {
		t.Fatalf("%v", err)
	}
}
