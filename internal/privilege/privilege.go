// Package privilege implements application-managed PUID/PGID startup.
//
// The daemon starts as root inside its container, takes ownership of the
// application's writable directories, and then permanently drops root
// privileges to the requested unix user and group before doing any work. This
// keeps the image free of a shell or gosu-style entrypoint while still letting
// operators match the container to the host volume's ownership through PUID
// and PGID.
package privilege

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Environment variables that request the runtime identity.
const (
	UserEnvVar  = "PUID"
	GroupEnvVar = "PGID"
)

// Identity is the unix user and group the process should run as after startup.
type Identity struct {
	UID int
	GID int
}

// Root reports whether the identity explicitly requests running as root.
// Callers treat this as an opt-out of privilege dropping.
func (i Identity) Root() bool {
	return i.UID == 0 && i.GID == 0
}

// FromEnvironment reads PUID and PGID. ok is false when neither is set, in
// which case the process keeps the identity it started with. A half-configured
// pair, an empty or non-numeric value, or a mixed root/non-root pair is an
// error, so a typo can never silently run the daemon as the wrong user.
func FromEnvironment() (Identity, bool, error) {
	return parse(os.LookupEnv)
}

func parse(lookup func(string) (string, bool)) (Identity, bool, error) {
	rawUID, uidSet := lookup(UserEnvVar)
	rawGID, gidSet := lookup(GroupEnvVar)
	if !uidSet && !gidSet {
		return Identity{}, false, nil
	}
	if !uidSet || !gidSet {
		return Identity{}, false, fmt.Errorf("%s and %s must be set together", UserEnvVar, GroupEnvVar)
	}

	uid, err := parseID(UserEnvVar, rawUID)
	if err != nil {
		return Identity{}, false, err
	}
	gid, err := parseID(GroupEnvVar, rawGID)
	if err != nil {
		return Identity{}, false, err
	}
	if (uid == 0) != (gid == 0) {
		return Identity{}, false, fmt.Errorf("%s and %s must both be 0 or both be non-zero", UserEnvVar, GroupEnvVar)
	}
	return Identity{UID: uid, GID: gid}, true, nil
}

func parseID(name, raw string) (int, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return 0, fmt.Errorf("%s is empty", name)
	}
	id, err := strconv.Atoi(value)
	if err != nil || id < 0 {
		return 0, fmt.Errorf("%s '%s' is not a valid id", name, value)
	}
	return id, nil
}
