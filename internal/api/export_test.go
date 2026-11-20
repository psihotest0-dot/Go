package api

import "testing"

// UseTempUploadDir points the upload folder at a temporary directory for the test and
// returns it, so that a test of another package's view of the API leaves nothing in the
// repository.
func UseTempUploadDir(t testing.TB) string {
	t.Helper()
	previous := uploadDir
	uploadDir = t.TempDir()
	t.Cleanup(func() { uploadDir = previous })
	return uploadDir
}
