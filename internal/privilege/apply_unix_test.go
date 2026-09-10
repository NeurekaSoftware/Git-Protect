//go:build unix

package privilege

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestChownTreeLeavesOwnedEntries(t *testing.T) {
	root := t.TempDir()
	nested := filepath.Join(root, "repositories", "example.git")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(nested, "HEAD")
	if err := os.WriteFile(target, []byte("ref: refs/heads/main\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	uid, gid := os.Geteuid(), os.Getegid()
	if err := chownTree(root, uid, gid); err != nil {
		t.Fatalf("chownTree() error = %v, want nil", err)
	}

	info, err := os.Lstat(target)
	if err != nil {
		t.Fatal(err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatalf("file info type = %T, want *syscall.Stat_t", info.Sys())
	}
	if int(stat.Uid) != uid || int(stat.Gid) != gid {
		t.Errorf("ownership = %d:%d, want %d:%d", stat.Uid, stat.Gid, uid, gid)
	}
}
