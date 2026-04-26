package baremetal

import (
	"sort"
	"strings"
)

// envSetter is the subset of *ssh.Session that applyEnv uses. Defined as an
// interface so tests can substitute a recorder without spinning up a real
// session.
type envSetter interface {
	Setenv(name, value string) error
}

// buildSudoCmd constructs the remote command line for executing userCmd as
// root with envOverlay applied to the sudo'd process.
//
// Behavior depends on whether SSH "env" requests are accepted by the server:
//
//   - When sess accepts Setenv for every overlay var (and forceFallback is
//     false), the variables are set on the SSH session and sudo is invoked
//     with --preserve-env=K1,K2,... so they survive into the privileged
//     process. The returned command line does NOT contain the values, so
//     captured stdout/stderr from misbehaving programs cannot leak them
//     through any echo of the command line itself.
//
//   - When the server rejects any Setenv (or forceFallback is true), the
//     values are passed as positional `KEY=VALUE` arguments to sudo. Values
//     are shell-quoted so secrets containing spaces, quotes, or globs are
//     transmitted verbatim. Note: an attacker who can read the *remote*
//     process table will see these values; for plaintext secret material
//     SSH Setenv is preferred — which is why we attempt it first.
//
// fallbackTriggered reports whether any Setenv call was rejected so the
// caller can sticky the fallback for the rest of the connection's lifetime.
func buildSudoCmd(sess envSetter, userCmd string, envOverlay map[string]string, forceFallback bool) (cmdLine string, fallbackTriggered bool, err error) {
	keys := sortedKeys(envOverlay)

	if !forceFallback && len(keys) > 0 {
		ok := true
		for _, k := range keys {
			if e := sess.Setenv(k, envOverlay[k]); e != nil {
				ok = false
				break
			}
		}
		if ok {
			// Use --preserve-env so sudo lets the SSH-set vars through.
			var b strings.Builder
			b.WriteString("sudo -n")
			b.WriteString(" --preserve-env=")
			for i, k := range keys {
				if i > 0 {
					b.WriteByte(',')
				}
				b.WriteString(k)
			}
			b.WriteString(" -- /bin/sh -c ")
			b.WriteString(shellQuote(userCmd))
			return b.String(), false, nil
		}
		// Setenv rejected — fall through to positional form.
	}

	var b strings.Builder
	b.WriteString("sudo -n")
	for _, k := range keys {
		b.WriteByte(' ')
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(shellQuote(envOverlay[k]))
	}
	b.WriteString(" -- /bin/sh -c ")
	b.WriteString(shellQuote(userCmd))

	if len(keys) == 0 {
		return b.String(), false, nil
	}
	return b.String(), true, nil
}

func sortedKeys(m map[string]string) []string {
	if len(m) == 0 {
		return nil
	}
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// shellQuote returns a POSIX-shell-safe single-quoted form of s. Single
// quotes inside s are escaped as '\”.
func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	if isShellSafe(s) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s) + 2)
	b.WriteByte('\'')
	for i := 0; i < len(s); i++ {
		if s[i] == '\'' {
			b.WriteString(`'\''`)
			continue
		}
		b.WriteByte(s[i])
	}
	b.WriteByte('\'')
	return b.String()
}

// isShellSafe reports whether s is composed entirely of characters that need
// no quoting in a POSIX shell.
func isShellSafe(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z',
			c >= 'A' && c <= 'Z',
			c >= '0' && c <= '9':
			continue
		}
		switch c {
		case '/', '.', '-', '_', ':', ',', '+', '=', '@', '%':
			continue
		}
		return false
	}
	return true
}
