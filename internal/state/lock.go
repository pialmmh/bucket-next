package state

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// lockDir takes the state directory's lock: one process at a time may open a shard's state
// for writing. The service holds it for its whole life, so an offline tool that would change
// the files is refused while the service runs (its next write would undo the change), and a
// second service on the same directory refuses to start.
func lockDir(dir string) (*os.File, error) {
	path := filepath.Join(dir, lockFile)
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o640)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%w: state_dir %s does not exist", ErrNotFound, dir)
	}
	if err != nil {
		return nil, fmt.Errorf("open lock %s: %w", path, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("another process holds the state in %s (is bucket-next running on it?) — "+
				"stop it first: two writers would undo each other's marks", dir)
		}
		return nil, fmt.Errorf("lock %s: %w", path, err)
	}
	return f, nil
}
