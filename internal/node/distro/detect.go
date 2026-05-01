package distro

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io/fs"
	"strings"
)

const osReleasePath = "/etc/os-release"

// Detect reads /etc/os-release via fsys and returns the appropriate Distro.
//
// Detection logic:
//   - Parses the "ID=" line from /etc/os-release.
//   - Returns Flatcar when ID=flatcar.
//   - Returns Ubuntu when ID=ubuntu.
//   - Returns Ubuntu (as the safe default) when the file is missing,
//     the ID line is absent, or the ID value is unrecognised. This
//     preserves existing behaviour for conventional Debian/Ubuntu hosts.
//
// The runner parameter is accepted for interface symmetry with other
// subsystem Detect functions but is not used; detection is pure filesystem
// I/O.
func Detect(_ context.Context, _ Runner, fsys FS) (Distro, error) {
	data, err := fsys.ReadFile(osReleasePath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			// File missing: fall back to Ubuntu (safe default).
			return &Ubuntu{}, nil
		}
		return nil, err
	}

	id := parseOSReleaseID(data)
	switch id {
	case "flatcar":
		return &Flatcar{}, nil
	case "ubuntu":
		return &Ubuntu{}, nil
	default:
		// Unrecognised or absent ID: default to Ubuntu.
		return &Ubuntu{}, nil
	}
}

// parseOSReleaseID scans os-release content for the "ID=" key and returns its
// unquoted value (lower-cased). Returns "" when no ID line is found.
func parseOSReleaseID(data []byte) string {
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "ID=") {
			continue
		}
		val := strings.TrimPrefix(line, "ID=")
		// Strip optional surrounding quotes (single or double).
		val = strings.Trim(val, `"'`)
		return strings.ToLower(strings.TrimSpace(val))
	}
	return ""
}
