package db

import "testing"

func TestDatabaseCloseHandlesMissingHandles(t *testing.T) {
	// A partially initialised Database (e.g. after a failed Connect) must not
	// panic on shutdown.
	var nilDatabase *Database
	nilDatabase.Close()

	(&Database{}).Close()

	// Close is documented as idempotent as well, since callers may also close
	// the pool themselves in deferred cleanup.
	database := &Database{}
	database.Close()
	database.Close()
}
