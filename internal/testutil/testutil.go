// Package testutil holds helpers shared by tests.
package testutil

import (
	"os"
	"testing"
)

// TempDir returns a per-test directory, on tmpfs (/dev/shm) when available. The state store
// fsyncs every write; on a real disk that costs milliseconds, which makes stress tests that
// reserve thousands of segments take minutes.
func TempDir(t testing.TB) string {
	t.Helper()
	if fi, err := os.Stat("/dev/shm"); err == nil && fi.IsDir() {
		if dir, err := os.MkdirTemp("/dev/shm", "bucket-next-test-*"); err == nil {
			t.Cleanup(func() { os.RemoveAll(dir) })
			return dir
		}
	}
	return t.TempDir()
}
