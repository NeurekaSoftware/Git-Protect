//go:build unix

package privilege

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

// Apply prepares paths for the requested identity and then permanently drops
// root privileges to it. Paths must be application-owned runtime directories;
// ownership changes are limited to each path and its contents.
//
// A process that already runs as the requested identity needs no work, so an
// operator-forced user: in Compose stays supported. A process that cannot
// reach the requested identity fails instead of running with the wrong
// ownership.
func Apply(identity Identity, paths ...string) error {
	if identity.Root() {
		return nil
	}
	if os.Geteuid() != 0 {
		if os.Geteuid() == identity.UID && os.Getegid() == identity.GID {
			return nil
		}
		return fmt.Errorf("cannot drop to uid %d gid %d: process is not root", identity.UID, identity.GID)
	}

	for _, path := range paths {
		if err := chownTree(path, identity.UID, identity.GID); err != nil {
			return err
		}
	}

	// Supplemental groups are replaced, the group id transitions before the
	// user id, and the final check fails startup if any step did not stick.
	if err := syscall.Setgroups([]int{identity.GID}); err != nil {
		return fmt.Errorf("set supplemental groups: %w", err)
	}
	if err := syscall.Setgid(identity.GID); err != nil {
		return fmt.Errorf("set group id: %w", err)
	}
	if err := syscall.Setuid(identity.UID); err != nil {
		return fmt.Errorf("set user id: %w", err)
	}
	if os.Geteuid() != identity.UID || os.Getegid() != identity.GID {
		return fmt.Errorf("privilege drop to uid %d gid %d did not take effect", identity.UID, identity.GID)
	}
	return nil
}

// chownTree changes ownership of root and everything beneath it. The paths
// passed by the daemon hold only the mirror cache it created itself, so a
// recursive pass is safe and lets a PUID/PGID change take effect without a
// manual chown. Symlinks are re-owned themselves rather than their targets.
func chownTree(root string, uid, gid int) error {
	return filepath.WalkDir(root, func(path string, _ fs.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("walk %s: %w", path, err)
		}
		if err := os.Lchown(path, uid, gid); err != nil {
			return fmt.Errorf("chown %s: %w", path, err)
		}
		return nil
	})
}
