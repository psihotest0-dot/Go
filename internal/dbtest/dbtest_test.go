package dbtest

import (
	"fmt"
	"runtime"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
)

// fakeTB records how ConnString ends: Fatalf and Skipf stop the goroutine the way
// the real *testing.T does, so ConnString has to be run in one of its own.
type fakeTB struct {
	testing.TB
	fatal   string
	skipped string
}

func (f *fakeTB) Helper() {}

func (f *fakeTB) Fatalf(format string, args ...any) {
	f.fatal = fmt.Sprintf(format, args...)
	runtime.Goexit()
}

func (f *fakeTB) Skipf(format string, args ...any) {
	f.skipped = fmt.Sprintf(format, args...)
	runtime.Goexit()
}

func resolve(t *testing.T) (string, *fakeTB) {
	t.Helper()
	tb := &fakeTB{}
	var (
		got string
		wg  sync.WaitGroup
	)
	wg.Add(1)
	go func() {
		defer wg.Done()
		got = ConnString(tb)
	}()
	wg.Wait()
	return got, tb
}

func TestDatabaseName(t *testing.T) {
	cases := map[string]string{
		"postgres://u:p@localhost:5432/app_test?sslmode=disable": "app_test",
		"postgres://localhost/postgres":                          "postgres",
		"postgres://localhost":                                   "",
		"://not a url":                                           "",
	}
	for conn, want := range cases {
		assert.Equal(t, want, DatabaseName(conn), conn)
	}
}

func TestConnStringSkipsWithoutTestDatabaseURL(t *testing.T) {
	t.Setenv(EnvVar, "")
	// DATABASE_URL is never a fallback, even when it is set.
	t.Setenv("DATABASE_URL", "postgres://localhost/app_test")

	got, tb := resolve(t)

	assert.Empty(t, got)
	assert.Contains(t, tb.skipped, EnvVar)
	assert.Empty(t, tb.fatal)
}

func TestConnStringRefusesTheLiveDatabase(t *testing.T) {
	const live = "postgres://postgres:postgres@localhost:5432/app_test"
	t.Setenv(EnvVar, live)
	t.Setenv("DATABASE_URL", live)

	got, tb := resolve(t)

	assert.Empty(t, got)
	assert.Contains(t, tb.fatal, "identical to DATABASE_URL")
}

func TestConnStringRefusesADatabaseWithoutTestInItsName(t *testing.T) {
	for _, conn := range []string{
		"postgres://postgres:postgres@localhost:5432/postgres",
		"postgres://localhost/production",
		"postgres://localhost",
		"://not a url",
	} {
		t.Setenv(EnvVar, conn)
		t.Setenv("DATABASE_URL", "")

		got, tb := resolve(t)

		assert.Empty(t, got, conn)
		assert.Contains(t, tb.fatal, "must contain", conn)
	}
}

func TestConnStringAcceptsADisposableDatabase(t *testing.T) {
	for _, conn := range []string{
		"postgres://postgres:postgres@localhost:5432/app_test?sslmode=disable",
		"postgres://localhost/TEST_run",
	} {
		t.Setenv(EnvVar, "  "+conn+"  ")
		t.Setenv("DATABASE_URL", "postgres://localhost/postgres")

		got, tb := resolve(t)

		assert.Equal(t, conn, got)
		assert.Empty(t, tb.fatal)
		assert.Empty(t, tb.skipped)
	}
}
