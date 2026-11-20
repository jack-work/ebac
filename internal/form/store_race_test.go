package form

import (
	"encoding/json"
	"fmt"
	"sync"
	"testing"
)

// ebacd runs several critics' rounds at once on ONE Store: each round
// remembers its critic key and records its form. Unsynchronized, that is a
// concurrent map write (it killed ebacd, 2026-11-19 22:54Z). RED at the
// unlocked code under -race (and usually a fatal "concurrent map writes"
// without it); GREEN with Store.mu.
func TestRememberIsSafeForConcurrentRounds(t *testing.T) {
	s := &Store{}
	const n = 64
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				s.remember(fmt.Sprintf("@f%02d", i), json.RawMessage(fmt.Sprintf(`{"name":"c%02d","round":%d}`, i, j)))
			}
		}(i)
	}
	wg.Wait()
	if len(s.loaded) != n {
		t.Fatalf("remembered %d forms, want %d", len(s.loaded), n)
	}
	if got := string(s.loaded["@f07"]); got != `{"name":"c07","round":49}` {
		t.Fatalf("@f07 = %s, want its last write", got)
	}
}

// The read side of the same maps: a round reads its merge base and its
// critic's form while other rounds write theirs.
func TestFormMapsReadAndWriteConcurrently(t *testing.T) {
	s := &Store{}
	const n = 32
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(2)
		name, id := fmt.Sprintf("c%02d", i), fmt.Sprintf("@f%02d", i)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				s.setForm(name, id)
				s.remember(id, json.RawMessage(`{"name":"`+name+`"}`))
			}
		}()
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				_ = s.base(id)
				_, _ = s.knownForm(name)
			}
		}()
	}
	wg.Wait()
	for i := 0; i < n; i++ {
		name, id := fmt.Sprintf("c%02d", i), fmt.Sprintf("@f%02d", i)
		if got, ok := s.knownForm(name); !ok || got != id {
			t.Fatalf("knownForm(%s) = %q %v, want %s", name, got, ok, id)
		}
		if got := string(s.base(id)); got != `{"name":"`+name+`"}` {
			t.Fatalf("base(%s) = %s", id, got)
		}
	}
	s.setForm("c00", "")
	if _, ok := s.knownForm("c00"); ok {
		t.Fatal("setForm(name, \"\") did not forget the form")
	}
}
