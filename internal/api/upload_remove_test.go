package api

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The path of the original comes from the database: only a file directly in the
// upload directory may be removed, whatever local_path says.
func TestRemoveOriginalOnlyRemovesFilesInTheUploadDirectory(t *testing.T) {
	withTempUploadDir(t)
	ctx := context.Background()
	write := func(path string) string {
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte("x"), 0o644))
		return path
	}

	inside := write(filepath.Join(uploadDir, "abc_book.epub"))
	removeOriginal(ctx, inside)
	assert.NoFileExists(t, inside)

	outside := write(filepath.Join(t.TempDir(), "elsewhere.epub"))
	removeOriginal(ctx, outside)
	assert.FileExists(t, outside, "a file outside the upload directory must be left alone")

	nested := write(filepath.Join(uploadDir, "sub", "nested.epub"))
	removeOriginal(ctx, nested)
	assert.FileExists(t, nested, "only files directly in the upload directory are originals")

	escaping := write(filepath.Join(filepath.Dir(uploadDir), "escaped.epub"))
	removeOriginal(ctx, filepath.Join(uploadDir, "..", "escaped.epub"))
	assert.FileExists(t, escaping, "a path that climbs out of the upload directory must be left alone")

	removeOriginal(ctx, filepath.Join(uploadDir, "already-gone.epub")) // no panic, nothing to do
}
