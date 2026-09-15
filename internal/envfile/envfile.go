// Package envfile parses the KEY=VALUE runtime credential files
// deploy/postgres/provision.sh and deploy/ministack/provision.sh write
// (deploy/ministack/.runtime/app-credentials.env,
// deploy/ministack/.runtime/test-credentials.env,
// deploy/postgres/.runtime/credentials.env). It is the single parser for
// that format, used by both production code (internal/pg, to read
// wallet_app's own Postgres password) and this repository's tests.
package envfile

import (
	"bufio"
	"fmt"
	"os"
	"regexp"
	"strings"
)

var keyPattern = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`)

// Read parses path into a map of KEY -> VALUE. Blank lines and lines
// starting with `#` are ignored. The key is whatever precedes the first
// `=` on a line and must match ^[A-Z_][A-Z0-9_]*$; the value is the exact
// remainder of the line, untrimmed and with no quote processing, so it may
// itself contain `=`, spaces or quotes.
//
// A missing file, a line with no `=`, or a line with an invalid key all
// return an error identifying path (and, for a malformed line, its line
// number) - never the value itself, so a malformed credential is never
// echoed back. The missing-file message names only path, with no
// suggestion specific to any one caller.
func Read(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("envfile: %s not found: %w", path, err)
	}
	defer f.Close()

	values := map[string]string{}
	scanner := bufio.NewScanner(f)
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}

		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("envfile: %s:%d: line has no '='", path, lineNo)
		}
		if !keyPattern.MatchString(key) {
			return nil, fmt.Errorf("envfile: %s:%d: invalid key %q", path, lineNo, key)
		}
		values[key] = value
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("envfile: read %s: %w", path, err)
	}
	return values, nil
}
