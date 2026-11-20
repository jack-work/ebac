package poll

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"github.com/gluck/ebac/sdk/model"
)

// lockRound takes the critic's round lock (dir/locks/<name>.lock, flock): ebacd
// and a hand `ebac poll` never run one critic's round at once. held says another
// process holds it (the caller skips its round). The lock dies with its process.
func lockRound(dir, name string) (unlock func(), held bool, err error) {
	if err := model.ValidName(name); err != nil {
		return nil, false, err
	}
	d := filepath.Join(dir, "locks")
	if err := os.MkdirAll(d, 0o700); err != nil {
		return nil, false, err
	}
	f, err := os.OpenFile(filepath.Join(d, name+".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, false, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, true, nil
		}
		return nil, false, fmt.Errorf("round lock %s: %w", name, err)
	}
	return func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); f.Close() }, false, nil
}
