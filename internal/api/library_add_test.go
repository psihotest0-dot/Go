package api_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go-monolith/internal/api"
	"go-monolith/internal/dbtest"
)

func filesInDir(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func TestLibraryAdderStoresTheDocumentAsAPendingFile(t *testing.T) {
	apiService, database, userID := setupAPI(t)
	dir := api.UseTempUploadDir(t)
	adder := apiService.NewLibraryAdder(nil)
	content := "%PDF-1.4\n" + strings.Repeat("a paper ", 50)

	added, err := adder.Add(context.Background(), userID, "Betting Against Beta.pdf", strings.NewReader(content))

	require.NoError(t, err)
	assert.False(t, added.Duplicate)
	assert.Equal(t, "Betting Against Beta.pdf", added.Name)
	assert.EqualValues(t, len(content), added.Size)

	files := listTestFiles(t, apiService, userID)
	require.Len(t, files, 1)
	assert.Equal(t, "Betting Against Beta.pdf", files[0].FileName)
	assert.Equal(t, "pending", files[0].Status, "the ingest has not run: it is waiting, not ready")

	var path string
	require.NoError(t, database.SqlDB.QueryRowContext(context.Background(),
		`SELECT local_path FROM ingested_files WHERE id = $1`, added.ID).Scan(&path))
	stored, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, content, string(stored), "the document is in the upload folder as it came")
	assert.Len(t, filesInDir(t, dir), 1)
}

// The same content twice is one file: the second call adds nothing and leaves no copy.
func TestLibraryAdderDoesNotAddTheSameContentTwice(t *testing.T) {
	apiService, _, userID := setupAPI(t)
	dir := api.UseTempUploadDir(t)
	adder := apiService.NewLibraryAdder(nil)

	first, err := adder.Add(context.Background(), userID, "a.pdf", strings.NewReader("%PDF-1.4 same"))
	require.NoError(t, err)
	second, err := adder.Add(context.Background(), userID, "other-name.pdf", strings.NewReader("%PDF-1.4 same"))

	require.NoError(t, err)
	assert.True(t, second.Duplicate)
	assert.Equal(t, first.ID, second.ID, "the file the user already has")
	assert.Len(t, listTestFiles(t, apiService, userID), 1)
	assert.Len(t, filesInDir(t, dir), 1, "the second copy was removed")
}

func TestLibraryAdderStopsAtTheUploadLimit(t *testing.T) {
	apiService, _, userID := setupAPI(t)
	apiService.WithUploadLimit(1000)
	dir := api.UseTempUploadDir(t)

	_, err := apiService.NewLibraryAdder(nil).Add(context.Background(), userID, "big.pdf", bytes.NewReader(bytes.Repeat([]byte("x"), 5000)))

	require.ErrorIs(t, err, api.ErrAddTooLarge)
	assert.Empty(t, listTestFiles(t, apiService, userID), "no row")
	assert.Empty(t, filesInDir(t, dir), "no file")
}

// A document that is exactly as large as the limit is taken.
func TestLibraryAdderTakesADocumentAsLargeAsTheLimit(t *testing.T) {
	apiService, _, userID := setupAPI(t)
	apiService.WithUploadLimit(1000)
	api.UseTempUploadDir(t)

	added, err := apiService.NewLibraryAdder(nil).Add(context.Background(), userID, "fit.pdf", bytes.NewReader(bytes.Repeat([]byte("x"), 1000)))

	require.NoError(t, err)
	assert.EqualValues(t, 1000, added.Size)
}

// The reader of the document fails in the middle (the connection broke, the document passed
// the limit of the fetcher): nothing is left, and the cause is still there for the caller.
func TestLibraryAdderLeavesNothingWhenTheReaderFails(t *testing.T) {
	apiService, _, userID := setupAPI(t)
	dir := api.UseTempUploadDir(t)
	broken := errors.New("the connection broke")
	src := io.MultiReader(strings.NewReader("%PDF-1.4 half"), &failingReader{err: broken})

	_, err := apiService.NewLibraryAdder(nil).Add(context.Background(), userID, "half.pdf", src)

	require.Error(t, err)
	assert.ErrorIs(t, err, broken)
	assert.Empty(t, listTestFiles(t, apiService, userID))
	assert.Empty(t, filesInDir(t, dir))
}

func TestLibraryAdderRefusesAnEmptyDocument(t *testing.T) {
	apiService, _, userID := setupAPI(t)
	dir := api.UseTempUploadDir(t)

	_, err := apiService.NewLibraryAdder(nil).Add(context.Background(), userID, "empty.pdf", strings.NewReader(""))

	require.ErrorIs(t, err, api.ErrAddEmpty)
	assert.Empty(t, filesInDir(t, dir))
}

func TestLibraryAdderKeepsUsersApart(t *testing.T) {
	apiService, database, userID := setupAPI(t)
	api.UseTempUploadDir(t)
	other, err := database.Queries.CreateUser(context.Background())
	require.NoError(t, err)
	t.Cleanup(func() { dbtest.DeleteUser(t, database.Pool, other.ID) })
	adder := apiService.NewLibraryAdder(nil)

	mine, err := adder.Add(context.Background(), userID, "a.pdf", strings.NewReader("%PDF-1.4 shared content"))
	require.NoError(t, err)
	theirs, err := adder.Add(context.Background(), other.ID, "a.pdf", strings.NewReader("%PDF-1.4 shared content"))

	require.NoError(t, err)
	assert.False(t, theirs.Duplicate, "another user's copy of the same text is not this user's")
	assert.NotEqual(t, mine.ID, theirs.ID)
}

type failingReader struct{ err error }

func (f *failingReader) Read([]byte) (int, error) { return 0, f.err }
