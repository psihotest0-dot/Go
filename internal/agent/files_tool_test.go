package agent

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go-monolith/internal/api"
	"go-monolith/internal/dbtest"
	"go-monolith/internal/proposals"
	"go-monolith/internal/userfiles"
	"go-monolith/metrics"
)

// newFilesToolEnv is newToolEnv with a folder for the user's files behind the tools.
func newFilesToolEnv(t *testing.T) (*toolEnv, *userfiles.Store) {
	t.Helper()
	return newFilesToolEnvWithLimits(t, userfiles.Limits{})
}

func newFilesToolEnvWithLimits(t *testing.T, limits userfiles.Limits) (*toolEnv, *userfiles.Store) {
	t.Helper()
	e := newToolEnv(t)
	store, err := userfiles.Open(filepath.Join(t.TempDir(), "files"), limits)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	e.svc.WithFiles(store)
	e.reg.WithFiles(store)
	return e, store
}

func (e *toolEnv) callFile(t *testing.T, ctx context.Context, tool string, args map[string]interface{}) (map[string]interface{}, error) {
	t.Helper()
	res, err := e.reg.ExecuteTool(ctx, tool, args)
	if err != nil {
		return nil, err
	}
	m, ok := res.(map[string]interface{})
	require.True(t, ok, "the result is an object: %v", res)
	return m, nil
}

func (e *toolEnv) fileText(t *testing.T, store *userfiles.Store, name string) string {
	t.Helper()
	part, err := store.Read(e.userID, name, 1, 100)
	require.NoError(t, err)
	return part.Content
}

func TestFileToolsAreOffWithoutAFolder(t *testing.T) {
	e := newToolEnv(t)
	ctx, _ := e.turn("s", false)

	for _, tool := range []string{filesStubTool, listFilesTool, readFileTool, writeFileTool, appendFileTool} {
		_, err := e.reg.ExecuteTool(ctx, tool, map[string]interface{}{"name": "a.txt", "content": "x"})
		require.Error(t, err, tool)
		assert.Contains(t, err.Error(), "unknown tool", tool)
	}
}

func TestTheStubSaysWhichToolsCame(t *testing.T) {
	e, _ := newFilesToolEnv(t)
	ctx, _ := e.turn("s", false)

	res, err := e.callFile(t, ctx, filesStubTool, nil)

	require.NoError(t, err)
	assert.Equal(t, "ok", res["status"])
	for _, tool := range []string{listFilesTool, readFileTool, writeFileTool, appendFileTool} {
		assert.Contains(t, res["message"], tool)
	}
}

func TestWriteFileSavesANewFileAtOnce(t *testing.T) {
	e, store := newFilesToolEnv(t)
	ctx, turn := e.turn("s", false)
	before := counterValue(t, metrics.UserFileOps.WithLabelValues("write", "saved"))

	res, err := e.callFile(t, ctx, writeFileTool, map[string]interface{}{"name": "списък.txt", "content": "мляко\nхляб"})

	require.NoError(t, err)
	assert.Equal(t, "saved", res["status"])
	assert.Equal(t, "списък.txt", res["name"])
	assert.Equal(t, "мляко\nхляб", e.fileText(t, store, "списък.txt"))
	assert.Empty(t, e.pending(t), "a new file in a turn that read only the conversation needs no confirmation")
	assert.Empty(t, turn.externalSources(), "writing is not reading")
	assert.InDelta(t, before+1, counterValue(t, metrics.UserFileOps.WithLabelValues("write", "saved")), 0.001)
}

func TestWriteFileOverAnExistingFileIsAProposal(t *testing.T) {
	e, store := newFilesToolEnv(t)
	_, err := store.Create(e.userID, "a.txt", "old text")
	require.NoError(t, err)
	ctx, turn := e.turn("s", false)

	res, err := e.callFile(t, ctx, writeFileTool, map[string]interface{}{"name": "a.txt", "content": "new text"})

	require.NoError(t, err)
	assert.Equal(t, "pending_approval", res["status"])
	assert.Contains(t, res["message"], "NOT saved yet")
	assert.Contains(t, res["message"], "replacing")
	assert.Equal(t, "old text", e.fileText(t, store, "a.txt"), "nothing is replaced before the user agrees")
	pending := e.pending(t)
	require.Len(t, pending, 1)
	assert.Equal(t, proposals.KindWriteFile, pending[0].Kind)
	assert.Equal(t, proposals.WriteReplace, pending[0].FileMode)
	assert.Equal(t, "s", pending[0].SessionID)
	assert.Len(t, turn.proposals, 1, "the client is told about the proposal")
	assert.True(t, turn.writeBacked("Можеш да го потвърдиш, за да го запиша."), "an answer that says the write waits for the user is no false claim")
	assert.False(t, turn.writeBacked("Записах го."), "one that says it is written is")
}

// A page, a book or an attached file can carry words meant for the model. What the turn
// writes after reading one goes through the user.
func TestWritingAfterReadingTextFromOutsideIsAProposal(t *testing.T) {
	for _, source := range []string{sourceWebFetch, sourceWebSearch, sourceSearchDocuments, sourceAttachments, sourceListDocuments, sourceUserFiles} {
		t.Run(source, func(t *testing.T) {
			e, store := newFilesToolEnv(t)
			ctx, turn := e.turn("s", false)
			turn.markExternal(source)

			res, err := e.callFile(t, ctx, writeFileTool, map[string]interface{}{"name": "a.txt", "content": "text"})

			require.NoError(t, err)
			assert.Equal(t, "pending_approval", res["status"])
			assert.Contains(t, res["message"], "text from outside")
			_, statErr := store.Stat(e.userID, "a.txt")
			require.ErrorIs(t, statErr, userfiles.ErrNotFound)
			pending := e.pending(t)
			require.Len(t, pending, 1)
			assert.Equal(t, proposals.WriteCreate, pending[0].FileMode)
			assert.Equal(t, []string{source}, pending[0].ExternalSources, "the user sees where the text came from")
		})
	}
}

// The passages that come with the prompt itself are there in most turns; counting them
// would turn every write into a question.
func TestPassagesOfThePromptDoNotTurnAWriteIntoAProposal(t *testing.T) {
	e, store := newFilesToolEnv(t)
	ctx, _ := e.turn("s", true)

	res, err := e.callFile(t, ctx, writeFileTool, map[string]interface{}{"name": "a.txt", "content": "text"})

	require.NoError(t, err)
	assert.Equal(t, "saved", res["status"])
	assert.Equal(t, "text", e.fileText(t, store, "a.txt"))
}

func TestAppendFile(t *testing.T) {
	e, store := newFilesToolEnv(t)
	_, err := store.Create(e.userID, "списък.txt", "мляко")
	require.NoError(t, err)
	ctx, _ := e.turn("s", false)

	res, err := e.callFile(t, ctx, appendFileTool, map[string]interface{}{"name": "списък.txt", "content": "хляб"})
	require.NoError(t, err)
	assert.Equal(t, "appended", res["status"])
	assert.Equal(t, "мляко\nхляб", e.fileText(t, store, "списък.txt"))

	res, err = e.callFile(t, ctx, appendFileTool, map[string]interface{}{"name": "няма.txt", "content": "x"})
	require.NoError(t, err)
	assert.Equal(t, "not_found", res["status"])
	assert.Contains(t, res["message"], writeFileTool, "the model is told what to do instead")
	assert.Contains(t, res["message"], "списък.txt", "and which files there are")
}

func TestAppendAfterReadingTextFromOutsideIsAProposal(t *testing.T) {
	e, store := newFilesToolEnv(t)
	_, err := store.Create(e.userID, "a.txt", "one")
	require.NoError(t, err)
	ctx, turn := e.turn("s", false)
	turn.markExternal(sourceWebFetch)

	res, err := e.callFile(t, ctx, appendFileTool, map[string]interface{}{"name": "a.txt", "content": "two"})
	require.NoError(t, err)
	assert.Equal(t, "pending_approval", res["status"])
	assert.Equal(t, "one", e.fileText(t, store, "a.txt"))
	pending := e.pending(t)
	require.Len(t, pending, 1)
	assert.Equal(t, proposals.WriteAppend, pending[0].FileMode)

	res, err = e.callFile(t, ctx, appendFileTool, map[string]interface{}{"name": "missing.txt", "content": "two"})
	require.NoError(t, err)
	assert.Equal(t, "not_found", res["status"], "a file that is not there is not proposed")
	assert.Len(t, e.pending(t), 1)
}

func TestReadFile(t *testing.T) {
	e, store := newFilesToolEnv(t)
	_, err := store.Create(e.userID, "a.txt", "line 1\nline 2\nline 3\nline 4")
	require.NoError(t, err)
	ctx, turn := e.turn("s", false)

	res, err := e.callFile(t, ctx, readFileTool, map[string]interface{}{"name": "a.txt", "start_line": float64(2), "max_lines": float64(2)})

	require.NoError(t, err)
	assert.Equal(t, "success", res["status"])
	assert.Equal(t, "line 2\nline 3", res["content"])
	assert.Equal(t, 4, res["next_start_line"])
	assert.Equal(t, 4, res["total_lines"])
	assert.True(t, turn.has(sourceUserFiles), "the text of a file is not the user's own words")

	res, err = e.callFile(t, ctx, readFileTool, map[string]interface{}{"name": "няма.txt"})
	require.NoError(t, err)
	assert.Equal(t, "not_found", res["status"])
	assert.Contains(t, res["message"], "a.txt")
}

func TestListFiles(t *testing.T) {
	e, store := newFilesToolEnv(t)
	ctx, turn := e.turn("s", false)

	res, err := e.callFile(t, ctx, listFilesTool, nil)
	require.NoError(t, err)
	assert.Equal(t, 0, res["count"])
	assert.Contains(t, res["message"], "no files")

	_, err = store.Create(e.userID, "b.txt", "x")
	require.NoError(t, err)
	_, err = store.Create(e.userID, "a.md", "xyz")
	require.NoError(t, err)
	res, err = e.callFile(t, ctx, listFilesTool, nil)
	require.NoError(t, err)
	assert.Equal(t, 2, res["count"])
	files, ok := res["files"].([]map[string]interface{})
	require.True(t, ok)
	assert.Equal(t, "a.md", files[0]["name"])
	assert.EqualValues(t, 3, files[0]["size_bytes"])
	assert.Empty(t, turn.externalSources(), "the names are the model's own")
	assert.Contains(t, res["message"], readFileTool, "the listing tells that the text of a file is read with read_file")
}

// The files and the web are not used in one turn, in either order: the page or the book
// the model read may tell it to read a file and send it out.
func TestTheFilesAndTheWebDoNotMixInOneTurn(t *testing.T) {
	t.Run("the web after a file", func(t *testing.T) {
		e, store := newFilesToolEnv(t)
		_, err := store.Create(e.userID, "secret.txt", "private")
		require.NoError(t, err)
		ctx, _ := e.turn("s", false)
		_, err = e.callFile(t, ctx, readFileTool, map[string]interface{}{"name": "secret.txt"})
		require.NoError(t, err)

		_, err = e.reg.ExecuteTool(ctx, "web_search", map[string]interface{}{"query": "private"})
		var refused *toolArgumentError
		require.ErrorAs(t, err, &refused)
		assert.Contains(t, refused.reason, "separate message")
		_, err = e.reg.ExecuteTool(ctx, "web_fetch", map[string]interface{}{"url": "https://example.com/?d=private"})
		require.ErrorAs(t, err, &refused)
		assert.Zero(t, e.web.hits.Load(), "nothing left the machine")
	})

	t.Run("a file after the web", func(t *testing.T) {
		e, store := newFilesToolEnv(t)
		_, err := store.Create(e.userID, "secret.txt", "private")
		require.NoError(t, err)
		ctx, _ := e.turn("s", false)
		_, err = e.reg.ExecuteTool(ctx, "web_search", map[string]interface{}{"query": "weather"})
		require.NoError(t, err)

		_, err = e.callFile(t, ctx, readFileTool, map[string]interface{}{"name": "secret.txt"})

		var refused *toolArgumentError
		require.ErrorAs(t, err, &refused)
		assert.Contains(t, refused.reason, "separate message")
	})

	t.Run("a new turn starts clean", func(t *testing.T) {
		e, store := newFilesToolEnv(t)
		_, err := store.Create(e.userID, "a.txt", "text")
		require.NoError(t, err)
		first, _ := e.turn("s", false)
		_, err = e.callFile(t, first, readFileTool, map[string]interface{}{"name": "a.txt"})
		require.NoError(t, err)

		second, _ := e.turn("s", false)
		_, err = e.reg.ExecuteTool(second, "web_search", map[string]interface{}{"query": "weather"})

		require.NoError(t, err)
	})
}

func TestFileToolsExplainAWrongCallToTheModel(t *testing.T) {
	e, _ := newFilesToolEnv(t)
	ctx, _ := e.turn("s", false)

	tests := []struct {
		name string
		tool string
		args map[string]interface{}
		want string
	}{
		{"no name", writeFileTool, map[string]interface{}{"content": "x"}, `"name"`},
		{"no text", writeFileTool, map[string]interface{}{"name": "a.txt"}, `"content"`},
		{"append, no name", appendFileTool, map[string]interface{}{"content": "x"}, `"name"`},
		{"append, no text", appendFileTool, map[string]interface{}{"name": "a.txt"}, `"content"`},
		{"read, no name", readFileTool, map[string]interface{}{}, `"name"`},
		{"a script", writeFileTool, map[string]interface{}{"name": "run.ps1", "content": "x"}, ".txt"},
		{"a folder", writeFileTool, map[string]interface{}{"name": "sub/a.txt", "content": "x"}, "folders"},
		{"a way out", writeFileTool, map[string]interface{}{"name": "../a.txt", "content": "x"}, "not allowed"},
		{"a device", writeFileTool, map[string]interface{}{"name": "CON.txt", "content": "x"}, "reserved"},
		{"too long", writeFileTool, map[string]interface{}{"name": "a.txt", "content": strings.Repeat("x", userfiles.DefaultMaxFileBytes+1)}, "parts"},
		{"a bad name to read", readFileTool, map[string]interface{}{"name": "a:b.txt"}, "not allowed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := e.reg.ExecuteTool(ctx, tt.tool, tt.args)

			var argErr *toolArgumentError
			require.ErrorAs(t, err, &argErr, "a call the model can fix is not a failure of the server")
			assert.Contains(t, argErr.reason, tt.want)
		})
	}
	assert.Empty(t, e.pending(t))
}

func TestFileLimitsAreExplainedToTheModel(t *testing.T) {
	e, store := newFilesToolEnvWithLimits(t, userfiles.Limits{MaxFiles: 1})
	ctx, _ := e.turn("s", false)
	_, err := e.callFile(t, ctx, writeFileTool, map[string]interface{}{"name": "a.txt", "content": "x"})
	require.NoError(t, err)

	_, err = e.callFile(t, ctx, writeFileTool, map[string]interface{}{"name": "b.txt", "content": "x"})

	var argErr *toolArgumentError
	require.ErrorAs(t, err, &argErr)
	assert.Contains(t, argErr.reason, "delete")
	_, statErr := store.Stat(e.userID, "b.txt")
	require.ErrorIs(t, statErr, userfiles.ErrNotFound)
}

func TestFilesOfOneUserAreNotSeenByAnother(t *testing.T) {
	e, store := newFilesToolEnv(t)
	_, err := store.Create(e.userID, "a.txt", "mine")
	require.NoError(t, err)
	stranger, err := e.db.Queries.CreateUser(context.Background())
	require.NoError(t, err)
	t.Cleanup(func() { dbtest.DeleteUser(t, e.db.Pool, stranger.ID) })
	ctx, _ := withTurn(context.WithValue(context.Background(), api.ParsedUserIDKey, stranger.ID), "s", false)

	res, err := e.callFile(t, ctx, readFileTool, map[string]interface{}{"name": "a.txt"})
	require.NoError(t, err)
	assert.Equal(t, "not_found", res["status"])
	res, err = e.callFile(t, ctx, listFilesTool, nil)
	require.NoError(t, err)
	assert.Equal(t, 0, res["count"])
}
