// Package dbtest resolves the disposable database the integration tests run
// against.
//
// The tests in this repository create and delete users, and some of them drop
// every table (goose.DownTo(..., 0)). Pointing them at the live DATABASE_URL
// once emptied the whole library: a test cleanup executed `DELETE FROM users`,
// and every table that stores user data cascades from there (see
// docs/incident-2026-10-30/REPORT.md). The helpers below make that mistake
// impossible: they read TEST_DATABASE_URL only and they refuse a connection
// whose database name does not look disposable.
package dbtest

import (
	"net/url"
	"os"
	"strings"
	"testing"
)

// EnvVar is the environment variable the integration tests read. DATABASE_URL
// is intentionally never consulted, so an export of the live connection string
// cannot leak into a destructive test.
const EnvVar = "TEST_DATABASE_URL"

// ConnString returns the TEST_DATABASE_URL connection string. It skips the
// test when the variable is unset and fails it when the target does not look
// like a throwaway database, so a misconfigured environment can never delete
// real rows.
func ConnString(t testing.TB) string {
	t.Helper()

	connString := strings.TrimSpace(os.Getenv(EnvVar))
	if connString == "" {
		t.Skipf("%s is not set: integration tests need a disposable database and never fall back to DATABASE_URL", EnvVar)
	}

	if live := strings.TrimSpace(os.Getenv("DATABASE_URL")); live != "" && live == connString {
		t.Fatalf("%s is identical to DATABASE_URL; refusing to run a destructive test against the live database", EnvVar)
	}

	if name := DatabaseName(connString); !strings.Contains(strings.ToLower(name), "test") {
		t.Fatalf("%s points at database %q; the name must contain \"test\" so a run can never reach production data", EnvVar, name)
	}

	return connString
}

// DatabaseName extracts the database name from a postgres connection URL. It
// returns an empty string when the URL cannot be parsed, which makes
// ConnString reject it.
func DatabaseName(connString string) string {
	parsed, err := url.Parse(connString)
	if err != nil {
		return ""
	}
	return strings.TrimPrefix(parsed.Path, "/")
}
