//go:build unix

package privilege

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"syscall"
)

// Apply prepares paths for the requested identity and then permanently drops
// root privileges to it. Paths must be application-owned runtime directories;
// ownership changes are limited to each path and its contents.
//
// A process that already runs unprivileged keeps the identity its runtime
// forced: an operator-selected user: in Compose or docker run --user wins over
// PUID/PGID, which cannot be applied without root.
func Apply(identity Identity, paths ...string) error {
	if identity.Root() {
		return nil
	}
	if os.Geteuid() != 0 {
		if os.Geteuid() != identity.UID || os.Getegid() != identity.GID {
			slog.Warn("PUID/PGID do not match the user the container started as; keeping the started identity.",
				"puid", identity.UID, "pgid", identity.GID,
				"uid", os.Geteuid(), "gid", os.Getegid())
		}
		return nil
	}

	for _, path := range paths {
		if err := chownTree(path, identity.UID, identity.GID); err != nil {
			return err
		}
	}

	// Supplemental groups are replaced, the group id transitions before the
	// user id, and the final check fails startup if any step did not stick.
	if err := syscall.Setgroups([]int{identity.GID}); err != nil {
		if !errors.Is(err, syscall.EPERM) {
			return fmt.Errorf("set supplemental groups: %w", err)
		}
		// A user namespace created with setgroups=deny rejects setgroups while
		// still allowing setgid and setuid; the runtime already fixed the list.
		slog.Warn("Could not set supplemental groups; the runtime controls them.", "error", err.Error())
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
	slog.Info("Privileges dropped.", "uid", identity.UID, "gid", identity.GID)
	return nil
}

// chownTree changes ownership of root and everything beneath it. Entries
// already owned by the target identity are skipped, so a volume that forbids
// chown (NFS root_squash, CIFS) stays usable when its data is already owned
// correctly. Symlinks are re-owned themselves rather than their targets.
func chownTree(root string, uid, gid int) error {
	return filepath.WalkDir(root, func(path string, _ fs.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("walk %s: %w", path, err)
		}
		info, err := os.Lstat(path)
		if err != nil {
			return fmt.Errorf("stat %s: %w", path, err)
		}
		if stat, ok := info.Sys().(*syscall.Stat_t); ok && int(stat.Uid) == uid && int(stat.Gid) == gid {
			return nil
		}
		if err := os.Lchown(path, uid, gid); err != nil {
			return fmt.Errorf("chown %s: %w", path, err)
		}
		return nil
	})
}
