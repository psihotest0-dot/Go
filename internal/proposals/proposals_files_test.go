package proposals

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go-monolith/internal/userfiles"
)

// newFilesEnv is newEnv with a folder for the user's files behind the service.
func newFilesEnv(t *testing.T) (*env, *userfiles.Store, string) {
	t.Helper()
	e := newEnv(t, nil)
	dir := filepath.Join(t.TempDir(), "files")
	store, err := userfiles.Open(dir, userfiles.Limits{})
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	e.svc.WithFiles(store)
	return e, store, dir
}

func (e *env) fileText(t *testing.T, store *userfiles.Store, name string) string {
	t.Helper()
	part, err := store.Read(e.userID, name, 1, 100)
	require.NoError(t, err)
	return part.Content
}

func TestProposedNewFileIsNotWrittenUntilApproved(t *testing.T) {
	e, store, _ := newFilesEnv(t)

	p, err := e.svc.ProposeWrite(e.ctx, e.userID, "s", WriteCreate, "list.txt", "мляко\nхляб", []string{"web_fetch"})
	require.NoError(t, err)
	assert.Equal(t, KindWriteFile, p.Kind)
	assert.Equal(t, "list.txt", p.FileName)
	assert.Equal(t, WriteCreate, p.FileMode)
	assert.Equal(t, "мляко\nхляб", p.Content)
	assert.Contains(t, p.Summary, "Нов файл „list.txt“")
	assert.Equal(t, []string{"web_fetch"}, p.ExternalSources)

	_, err = store.Stat(e.userID, "list.txt")
	require.ErrorIs(t, err, userfiles.ErrNotFound, "a proposal writes nothing")

	approved, err := e.svc.Approve(e.ctx, e.userID, p.ID)
	require.NoError(t, err)
	assert.Equal(t, p.ID, approved.ID)
	assert.Equal(t, "мляко\nхляб", e.fileText(t, store, "list.txt"))

	list, err := e.svc.Pending(e.ctx, e.userID)
	require.NoError(t, err)
	assert.Empty(t, list)
}

func TestRejectedFileProposalChangesNothing(t *testing.T) {
	e, store, _ := newFilesEnv(t)
	_, err := store.Create(e.userID, "a.txt", "keep me")
	require.NoError(t, err)
	p, err := e.svc.ProposeWrite(e.ctx, e.userID, "s", WriteReplace, "a.txt", "new text", nil)
	require.NoError(t, err)

	_, err = e.svc.Reject(e.ctx, e.userID, p.ID)
	require.NoError(t, err)

	assert.Equal(t, "keep me", e.fileText(t, store, "a.txt"))
	_, err = e.svc.Approve(e.ctx, e.userID, p.ID)
	require.ErrorIs(t, err, ErrNotPending, "a rejected proposal cannot be approved afterwards")
	assert.Equal(t, "keep me", e.fileText(t, store, "a.txt"))
}

func TestApprovedReplacementKeepsThePreviousVersion(t *testing.T) {
	e, store, _ := newFilesEnv(t)
	_, err := store.Create(e.userID, "a.txt", "old text")
	require.NoError(t, err)
	p, err := e.svc.ProposeWrite(e.ctx, e.userID, "s", WriteReplace, "a.txt", "new text", nil)
	require.NoError(t, err)
	assert.Contains(t, p.Summary, "Замяна на файла „a.txt“")
	assert.Equal(t, "old text", e.fileText(t, store, "a.txt"), "nothing is replaced before the user agrees")

	_, err = e.svc.Approve(e.ctx, e.userID, p.ID)
	require.NoError(t, err)

	assert.Equal(t, "new text", e.fileText(t, store, "a.txt"))
	prev, err := store.Previous(e.userID, "a.txt")
	require.NoError(t, err)
	assert.Equal(t, "old text", prev)
}

func TestApprovedAppendAddsToTheFile(t *testing.T) {
	e, store, _ := newFilesEnv(t)
	_, err := store.Create(e.userID, "a.txt", "one")
	require.NoError(t, err)
	p, err := e.svc.ProposeWrite(e.ctx, e.userID, "s", WriteAppend, "a.txt", "two", nil)
	require.NoError(t, err)
	assert.Contains(t, p.Summary, "Добавяне към файла „a.txt“")

	_, err = e.svc.Approve(e.ctx, e.userID, p.ID)
	require.NoError(t, err)

	assert.Equal(t, "one\ntwo", e.fileText(t, store, "a.txt"))
}

// The model is told now, while it can still tell the user, not when the user taps.
func TestFileProposalIsCheckedWhenItIsMade(t *testing.T) {
	e, store, _ := newFilesEnv(t)
	_, err := store.Create(e.userID, "taken.txt", "x")
	require.NoError(t, err)
	var invalid *InvalidError

	_, err = e.svc.ProposeWrite(e.ctx, e.userID, "s", WriteCreate, "../x.txt", "text", nil)
	require.ErrorAs(t, err, &invalid, "a name that leads out")
	_, err = e.svc.ProposeWrite(e.ctx, e.userID, "s", WriteCreate, "run.ps1", "text", nil)
	require.ErrorAs(t, err, &invalid, "a script")
	_, err = e.svc.ProposeWrite(e.ctx, e.userID, "s", WriteCreate, "a.txt", "  ", nil)
	require.ErrorAs(t, err, &invalid, "no text")
	_, err = e.svc.ProposeWrite(e.ctx, e.userID, "s", WriteCreate, "a.txt", strings.Repeat("x", userfiles.DefaultMaxFileBytes+1), nil)
	require.ErrorAs(t, err, &invalid, "too long")
	_, err = e.svc.ProposeWrite(e.ctx, e.userID, "s", "delete", "a.txt", "text", nil)
	require.ErrorAs(t, err, &invalid, "no such way of writing")

	_, err = e.svc.ProposeWrite(e.ctx, e.userID, "s", WriteCreate, "taken.txt", "text", nil)
	require.ErrorIs(t, err, userfiles.ErrExists)
	_, err = e.svc.ProposeWrite(e.ctx, e.userID, "s", WriteReplace, "missing.txt", "text", nil)
	require.ErrorIs(t, err, userfiles.ErrNotFound)
	_, err = e.svc.ProposeWrite(e.ctx, e.userID, "s", WriteAppend, "missing.txt", "text", nil)
	require.ErrorIs(t, err, userfiles.ErrNotFound)

	list, err := e.svc.Pending(e.ctx, e.userID)
	require.NoError(t, err)
	assert.Empty(t, list, "none of these left a proposal")
}

func TestFileProposalNeedsTheFolder(t *testing.T) {
	e := newEnv(t, nil)
	var invalid *InvalidError

	_, err := e.svc.ProposeWrite(e.ctx, e.userID, "s", WriteCreate, "a.txt", "text", nil)

	require.ErrorAs(t, err, &invalid)
	assert.Contains(t, invalid.Reason, "not enabled")
}

// Between the proposal and the answer the folder can change. The proposal then cannot be
// applied; it stays open, with a reason for the user, and nothing is lost.
func TestApprovalOfAFileProposalFailsWhenTheFolderChanged(t *testing.T) {
	t.Run("the new file's name is taken", func(t *testing.T) {
		e, store, _ := newFilesEnv(t)
		p, err := e.svc.ProposeWrite(e.ctx, e.userID, "s", WriteCreate, "a.txt", "from the proposal", nil)
		require.NoError(t, err)
		_, err = store.Create(e.userID, "a.txt", "made meanwhile")
		require.NoError(t, err)

		_, err = e.svc.Approve(e.ctx, e.userID, p.ID)

		var applyErr *ApplyError
		require.ErrorAs(t, err, &applyErr)
		assert.Contains(t, applyErr.Reason, "вече съществува")
		assert.Equal(t, "made meanwhile", e.fileText(t, store, "a.txt"), "the other file is not replaced")
		list, err := e.svc.Pending(e.ctx, e.userID)
		require.NoError(t, err)
		require.Len(t, list, 1, "the proposal stays open")
		_, err = e.svc.Reject(e.ctx, e.userID, p.ID)
		require.NoError(t, err)
	})

	t.Run("the file to change is gone", func(t *testing.T) {
		e, store, dir := newFilesEnv(t)
		_, err := store.Create(e.userID, "a.txt", "old")
		require.NoError(t, err)
		p, err := e.svc.ProposeWrite(e.ctx, e.userID, "s", WriteReplace, "a.txt", "new", nil)
		require.NoError(t, err)
		require.NoError(t, os.Remove(filepath.Join(dir, e.userID.String(), "a.txt")))

		_, err = e.svc.Approve(e.ctx, e.userID, p.ID)

		var applyErr *ApplyError
		require.ErrorAs(t, err, &applyErr)
		assert.Contains(t, applyErr.Reason, "вече го няма")
		_, statErr := store.Stat(e.userID, "a.txt")
		require.ErrorIs(t, statErr, userfiles.ErrNotFound, "a replacement does not make a file that was removed")
	})

	t.Run("there is no room", func(t *testing.T) {
		e := newEnv(t, nil)
		store, err := userfiles.Open(filepath.Join(t.TempDir(), "files"), userfiles.Limits{MaxFiles: 1})
		require.NoError(t, err)
		t.Cleanup(func() { _ = store.Close() })
		e.svc.WithFiles(store)
		p, err := e.svc.ProposeWrite(e.ctx, e.userID, "s", WriteCreate, "a.txt", "text", nil)
		require.NoError(t, err)
		_, err = store.Create(e.userID, "other.txt", "x")
		require.NoError(t, err)

		_, err = e.svc.Approve(e.ctx, e.userID, p.ID)

		var applyErr *ApplyError
		require.ErrorAs(t, err, &applyErr)
		assert.Contains(t, applyErr.Reason, "твърде много файлове")
	})
}

func TestFileProposalsBelongToTheirUser(t *testing.T) {
	e, store, _ := newFilesEnv(t)
	stranger := e.newUser(t)
	p, err := e.svc.ProposeWrite(e.ctx, e.userID, "s", WriteCreate, "a.txt", "text", nil)
	require.NoError(t, err)

	_, err = e.svc.Approve(e.ctx, stranger, p.ID)

	require.ErrorIs(t, err, ErrNotFound)
	_, err = store.Stat(stranger, "a.txt")
	require.ErrorIs(t, err, userfiles.ErrNotFound)
	_, err = store.Stat(e.userID, "a.txt")
	require.ErrorIs(t, err, userfiles.ErrNotFound, "the owner has not approved it either")
}

func TestSameFileProposalIsStoredOnce(t *testing.T) {
	e, _, _ := newFilesEnv(t)

	first, err := e.svc.ProposeWrite(e.ctx, e.userID, "s", WriteCreate, "a.txt", "text", nil)
	require.NoError(t, err)
	second, err := e.svc.ProposeWrite(e.ctx, e.userID, "s", WriteCreate, "a.txt", "text", nil)
	require.NoError(t, err)

	assert.Equal(t, first.ID, second.ID)
	list, err := e.svc.Pending(e.ctx, e.userID)
	require.NoError(t, err)
	assert.Len(t, list, 1)
}

func TestWriteSummaryShowsTheBeginningOfTheText(t *testing.T) {
	long := strings.Repeat("дума ", 100)
	summary := writeSummary(WritePayload{Mode: WriteCreate, Name: "a.txt", Content: long})

	assert.Contains(t, summary, fmt.Sprintf("(%d знака)", len([]rune(long))))
	assert.True(t, strings.HasSuffix(summary, "…"), "a long text is cut")
	assert.NotContains(t, summary, "\n")
	assert.Less(t, len([]rune(summary)), 260)

	one := writeSummary(WritePayload{Mode: WriteAppend, Name: "b.md", Content: "ред едно\n\nред две"})
	assert.Equal(t, "Добавяне към файла „b.md“ (17 знака): ред едно ред две", one, "line breaks become spaces")
}

func TestHTTPFileProposalThatCannotBeAppliedIs422(t *testing.T) {
	e, store, _ := newFilesEnv(t)
	p, err := e.svc.ProposeWrite(e.ctx, e.userID, "s", WriteCreate, "a.txt", "text", nil)
	require.NoError(t, err)
	_, err = store.Create(e.userID, "a.txt", "made meanwhile")
	require.NoError(t, err)

	rec := e.serve(t, e.userID, http.MethodPost, fmt.Sprintf("/proposals/%d/approve", p.ID))

	require.Equal(t, http.StatusUnprocessableEntity, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "вече съществува")

	rec = e.serve(t, e.userID, http.MethodGet, "/proposals")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `"file_name":"a.txt"`)
	assert.Contains(t, rec.Body.String(), `"file_mode":"create"`)

	rec = e.serve(t, e.userID, http.MethodPost, fmt.Sprintf("/proposals/%d/reject", p.ID))
	require.Equal(t, http.StatusOK, rec.Code)
}

func TestApplyErrorIsNotAServerFailure(t *testing.T) {
	err := fmt.Errorf("wrapped: %w", &ApplyError{Reason: "причина"})
	var applyErr *ApplyError
	assert.True(t, errors.As(err, &applyErr))
	assert.Equal(t, "причина", applyErr.Error())
}
