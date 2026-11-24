package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// Forms is the only side effect praxis has: read a form, patch one key,
// delete one key. FigaroForms implements it with the figaro CLI.
type Forms interface {
	// Get returns the form's state; ErrNoForm when it does not exist.
	Get(ctx context.Context, ref string) (map[string]any, error)
	// Set patches one key path with a JSON value.
	Set(ctx context.Context, ref, key string, value any) error
	// Delete removes one key path.
	Delete(ctx context.Context, ref, key string) error
}

// ErrNoForm is a ref figaro does not know.
var ErrNoForm = errors.New("no such form")

// FigaroForms runs the figaro CLI.
type FigaroForms struct{ Bin string }

func (f *FigaroForms) Get(ctx context.Context, ref string) (map[string]any, error) {
	out, err := exec.CommandContext(ctx, f.Bin, "form", "show", ref, "-j").Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && strings.Contains(strings.ToLower(string(ee.Stderr)), "not found") {
			return nil, fmt.Errorf("%s: %w", ref, ErrNoForm)
		}
		return nil, fmt.Errorf("figaro form show %s: %w", ref, err)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		return nil, fmt.Errorf("figaro form show %s: %w", ref, err)
	}
	return m, nil
}

func (f *FigaroForms) Set(ctx context.Context, ref, key string, value any) error {
	b, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if out, err := exec.CommandContext(ctx, f.Bin, "form", "set", "--id", ref, key, string(b)).CombinedOutput(); err != nil {
		return fmt.Errorf("figaro form set %s %s: %w: %s", ref, key, err, lastLine(out))
	}
	return nil
}

func (f *FigaroForms) Delete(ctx context.Context, ref, key string) error {
	if out, err := exec.CommandContext(ctx, f.Bin, "form", "delete", "--id", ref, key).CombinedOutput(); err != nil {
		return fmt.Errorf("figaro form delete %s %s: %w: %s", ref, key, err, lastLine(out))
	}
	return nil
}

func lastLine(b []byte) string {
	s := strings.TrimSpace(string(b))
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		return s[i+1:]
	}
	return s
}

// randomID is a request id: 12 lowercase hex.
func randomID() string {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// lookup walks a dotted path through nested maps (figaro forms nest keys).
func lookup(m map[string]any, path string) (any, bool) {
	var cur any = m
	for _, p := range strings.Split(path, ".") {
		mm, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		cur, ok = mm[p]
		if !ok {
			return nil, false
		}
	}
	return cur, true
}
