package forge

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// PullHeads speaks git protocol v2 ls-refs: one ref-prefix per PR, the token as basic auth, and reads the head shas.
func TestPullHeads(t *testing.T) {
	var gotBody, gotProto, gotPath, gotUser, gotPass string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody, gotProto, gotPath = string(b), r.Header.Get("Git-Protocol"), r.URL.Path
		gotUser, gotPass, _ = r.BasicAuth()
		io.WriteString(w, "003e3fdce1806b21348b50a91c13f0ab270edfb6191c refs/pull/1/head\n"+
			"0043ad68d83c0284b19f31c9b18144b212fec0b7a9f1 refs/pull/115513/head\n0000")
	}))
	defer srv.Close()
	g := &GH{Host: "microsoft.ghe.com", NoCred: true, GitBase: srv.URL}
	heads, err := g.PullHeads(context.Background(), "bic/aether", []int{1, 115513, 7})
	if err != nil {
		t.Fatal(err)
	}
	if heads[1] != "3fdce1806b21348b50a91c13f0ab270edfb6191c" || heads[115513] != "ad68d83c0284b19f31c9b18144b212fec0b7a9f1" || len(heads) != 2 {
		t.Fatalf("heads %v", heads)
	}
	want := "0014command=ls-refs\n0001" + "0020ref-prefix refs/pull/1/head\n" + "0025ref-prefix refs/pull/115513/head\n" +
		"0020ref-prefix refs/pull/7/head\n0000"
	if gotBody != want || gotProto != "version=2" || gotPath != "/bic/aether.git/git-upload-pack" || gotUser != "" || gotPass != "" {
		t.Fatalf("request %q proto %q path %q auth %q:%q", gotBody, gotProto, gotPath, gotUser, gotPass)
	}
	if gitHost("microsoft.ghe.com") != "api.microsoft.ghe.com" || gitHost("api.microsoft.ghe.com") != "api.microsoft.ghe.com" {
		t.Fatal("the git host is not the API host the egress proxy passes")
	}
}

func TestPullHeadsErrors(t *testing.T) {
	for name, c := range map[string]struct {
		code int
		body string
		want string
	}{
		"401":   {401, "Bad credentials", "HTTP 401: Bad credentials"},
		"ERR":   {200, "000eERR denied", "ls-refs: denied"},
		"short": {200, "003e3fdce18", "cut short"},
		"bad":   {200, "zzzz", "bad pkt-line"},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(c.code)
			io.WriteString(w, c.body)
		}))
		_, err := (&GH{NoCred: true, GitBase: srv.URL}).PullHeads(context.Background(), "o/r", []int{1})
		srv.Close()
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", name, err, c.want)
		}
	}
	if h, err := parseLsRefs(bufio.NewReader(strings.NewReader("0000"))); err != nil || len(h) != 0 {
		t.Fatalf("empty answer: %v %v", h, err)
	}
}

// The token rides as basic auth, never in the URL.
func TestPullHeadsAuth(t *testing.T) {
	var user, pass, url string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, _ = r.BasicAuth()
		url = r.URL.String()
		io.WriteString(w, "0000")
	}))
	defer srv.Close()
	if _, err := lsPullHeads(context.Background(), srv.Client(), srv.URL+"/o/r.git/git-upload-pack", "tok123", []int{1}); err != nil {
		t.Fatal(err)
	}
	if user != "x-access-token" || pass != "tok123" || strings.Contains(url, "tok123") {
		t.Fatalf("auth %q:%q url %q", user, pass, url)
	}
}
