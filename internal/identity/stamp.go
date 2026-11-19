package identity

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// A stamp is an HMAC over a verdict's fields with a key only uid ebac can
// read (0600 in its state dir). `ebac verdict` writes it after the role
// check; the keel plugin counts a verdict only when its stamp verifies, so a
// verdict set by hand on the form (by any seat) never counts.

// KeyPath is EBAC_STAMP_KEY, else $HOME/.local/state/ebac-stamp/stamp.key:
// a directory of its own, so the plugin sandbox can hide it whole.
func KeyPath() string {
	if p := os.Getenv("EBAC_STAMP_KEY"); p != "" {
		return p
	}
	return filepath.Join(os.Getenv("HOME"), ".local", "state", "ebac-stamp", "stamp.key")
}

// LoadKey reads the key; create makes one (32 random bytes, 0600) when absent.
func LoadKey(path string, create bool) ([]byte, error) {
	b, err := os.ReadFile(path)
	if err == nil {
		if len(b) < 32 {
			return nil, fmt.Errorf("stamp key %s: %d bytes, want 32", path, len(b))
		}
		return b, nil
	}
	if !errors.Is(err, os.ErrNotExist) || !create {
		return nil, fmt.Errorf("stamp key %s: %w", path, err)
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return LoadKey(path, false) // another ebac made it first
		}
		return nil, err
	}
	defer f.Close()
	if _, err := f.Write(key); err != nil {
		return nil, err
	}
	return key, nil
}

// Stamp is the hex HMAC-SHA256 of the fields, joined unambiguously.
func Stamp(key []byte, fields ...string) string {
	m := hmac.New(sha256.New, key)
	for _, f := range fields {
		fmt.Fprintf(m, "%d:%s;", len(f), f)
	}
	return hex.EncodeToString(m.Sum(nil))
}

// Verify says whether stamp is the stamp of fields under key.
func Verify(key []byte, stamp string, fields ...string) bool {
	if len(key) == 0 || stamp == "" {
		return false
	}
	want := Stamp(key, fields...)
	return hmac.Equal([]byte(strings.ToLower(stamp)), []byte(want))
}
