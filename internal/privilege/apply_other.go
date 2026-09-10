//go:build !unix

package privilege

import "fmt"

// Apply reports that PUID/PGID privilege dropping requires a unix host. An
// explicit root identity is a no-op, matching the unix implementation.
func Apply(identity Identity, _ ...string) error {
	if identity.Root() {
		return nil
	}
	return fmt.Errorf("cannot drop to uid %d gid %d: PUID/PGID is only supported on unix", identity.UID, identity.GID)
}
