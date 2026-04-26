package user

import (
	"fmt"
	"os/user"
	"strconv"
)

// lookupUserHome resolves a username to its home directory and numeric
// uid/gid using the standard library os/user package.
//
// Split into its own file so tests can inject a fake LookupUserHome via
// the FS interface without pulling in cgo-tinged paths on darwin.
func lookupUserHome(name string) (string, int, int, error) {
	u, err := user.Lookup(name)
	if err != nil {
		return "", 0, 0, err
	}
	uid, err := strconv.Atoi(u.Uid)
	if err != nil {
		return "", 0, 0, fmt.Errorf("parse uid %q: %w", u.Uid, err)
	}
	gid, err := strconv.Atoi(u.Gid)
	if err != nil {
		return "", 0, 0, fmt.Errorf("parse gid %q: %w", u.Gid, err)
	}
	return u.HomeDir, uid, gid, nil
}
