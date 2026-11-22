package form

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/gluck/ebac/sdk/model"
)

// A github-cache critic's write scope, fields and maintainer come from the trusted config only. A register
// config key that names a maintainer other than the trusted one, or any scope / write_fields / approve_prs, has no
// effect and is flagged; a config maintainer equal to the trusted one is not (control). With no trusted authority a
// form-held gh_write is cleared. RED if ghConfigIgnored goes (nothing flagged) or the least-authority clear of the
// write scope goes (the last row keeps the form's scope).
func TestGHCacheWriteScopeIsTrustedOnly(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "c.json")
	_ = os.WriteFile(p, []byte(`{"trusted":{"critics":{"@g1":{"authority":{"source":"github-cache","mode":"review","write":true,
		"scope":{"repos":["acme/widget"],"authors":["our-author"],"read_only_authors":["their-author"]},
		"write_fields":["labels","draft"],"maintainer":"@0000000a"}}}}}`), 0o600)
	ResetTrusted(p)
	t.Cleanup(func() { ResetTrusted(model.TrustedConfigPath) })
	old := authorityDir
	authorityDir = ""
	t.Cleanup(func() { authorityDir = old })
	seatCritic, _ := json.Marshal(map[string]any{"name": "ghc", "source": "github-cache", "mode": "review", "write": true, "maintainer": "@0000000a",
		"gh_write": map[string]any{"scope": map[string]any{"repos": []string{"acme/widget"}, "authors": []string{"our-author", "their-author"}}, "fields": model.GHWriteFields}})
	read := func(id, config string) *model.Critic {
		t.Helper()
		f := &CriticForm{ID: id, Keys: map[string]json.RawMessage{model.CriticKey: seatCritic, model.GHCacheConfigKey: json.RawMessage(config)}}
		w, err := f.Critic()
		if err != nil {
			t.Fatal(err)
		}
		return w
	}
	want := &model.GHWrite{Scope: model.GHScope{Repos: []string{"acme/widget"}, Authors: []string{"our-author"}, ReadOnlyAuthors: []string{"their-author"}},
		Fields: []string{"labels", "draft"}}
	for _, tc := range []struct {
		name, config, flagged string
	}{
		{"widening config", `{"repos":["acme/widget"],"rules":[{"author":["their-author"]}],"maintainer":"@evil","scope":{"repos":["acme/other"]},"write_fields":["title"],"approve_prs":["acme/widget#1@x"]}`,
			"gh_write,config.maintainer,config.scope,config.write_fields,config.approve_prs"},
		{"the trusted maintainer named in config", `{"repos":["acme/widget"],"rules":[{"author":["our-author"]}],"maintainer":"@0000000a"}`, "gh_write"},
		{"no maintainer in config", `{"repos":["acme/widget"],"rules":[{"author":["our-author"]}]}`, "gh_write"},
		{"maintainer not a string", `{"repos":["acme/widget"],"rules":[{"author":["our-author"]}],"maintainer":7}`, "gh_write,config.maintainer"},
	} {
		w := read("@g1", tc.config)
		if got := strings.Join(w.FormIgnored, ","); got != tc.flagged {
			t.Errorf("%s: flagged %q, want %q", tc.name, got, tc.flagged)
		}
		if !reflect.DeepEqual(w.GHWrite, want) || w.Maintainer != "@0000000a" {
			t.Errorf("%s: write authority %+v maintainer %q, want the trusted %+v @0000000a", tc.name, w.GHWrite, w.Maintainer, want)
		}
	}
	if w := read("@g2", `{"repos":["acme/widget"],"rules":[{"author":["our-author"]}]}`); !w.NoAuthority || w.GHWrite != nil || w.Write || w.Maintainer != "" {
		t.Errorf("no trusted authority: NoAuthority %v write %v gh_write %+v maintainer %q (want not run, nothing writable, nobody woken)", w.NoAuthority, w.Write, w.GHWrite, w.Maintainer)
	}
}
