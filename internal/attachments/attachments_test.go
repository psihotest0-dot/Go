package attachments

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go-monolith/internal/db"
	"go-monolith/internal/dbtest"
)

// ------------------------------------------------------------------ without a database
func TestCleanNameKeepsOnlyTheFileName(t *testing.T) {
	for in, want := range map[string]string{
		"main.go":                "main.go",
		`src/cmd\server/main.go`: "main.go",
		"  notes.txt  ":          "notes.txt",
		"with\ttab\nand.py":      "withtaband.py",
		"Dockerfile":             "Dockerfile",
		"справка.py":             "справка.py",
		strings.Repeat("я", 120): strings.Repeat("я", 120),
	} {
		got, err := CleanName(in)
		require.NoError(t, err, in)
		assert.Equal(t, want, got)
	}

	for _, bad := range []string{"", "   ", ".", "..", "dir/", `a\..`, "\x00\x01", strings.Repeat("я", 121)} {
		_, err := CleanName(bad)
		assert.ErrorIs(t, err, ErrBadName, "%q", bad)
	}
}

func TestNormalizeContentMakesLFTextWithoutBOM(t *testing.T) {
	got, err := NormalizeContent("\xef\xbb\xbfone\r\ntwo\rthree\n")
	require.NoError(t, err)
	assert.Equal(t, "one\ntwo\nthree\n", got)

	_, err = NormalizeContent(" \n\t\r\n")
	assert.ErrorIs(t, err, ErrEmpty)
	_, err = NormalizeContent("MZ\x00\x01binary")
	assert.ErrorIs(t, err, ErrBinary)
	_, err = NormalizeContent(strings.Repeat("a", MaxFileBytes+1))
	assert.ErrorIs(t, err, ErrTooLarge)
	_, err = NormalizeContent(strings.Repeat("a", MaxFileBytes))
	assert.NoError(t, err, "exactly the limit is allowed")
}

func TestSplitLinesIgnoresTheFinalLineBreak(t *testing.T) {
	assert.Equal(t, []string{"a", "b"}, SplitLines("a\nb\n"))
	assert.Equal(t, []string{"a", "b"}, SplitLines("a\nb"))
	assert.Equal(t, []string{"a", "", "b"}, SplitLines("a\n\nb"))
	assert.Empty(t, SplitLines(""))
}

func TestTokenEstimateIsOnTheSafeSideForCode(t *testing.T) {
	assert.Equal(t, 0, EstimateTokens(0))
	assert.Equal(t, 1, EstimateTokens(1))
	assert.Equal(t, 1, EstimateTokens(3))
	assert.Equal(t, 2, EstimateTokens(4))
	assert.Equal(t, 100, EstimateTokens(300))
}

func file(name, content string) File {
	return File{Info: Info{Name: name}, Content: content}
}

func TestInlineNumbersEveryLine(t *testing.T) {
	got := Inline(file("a.go", "package a\n\nfunc F() {}\n"))
	assert.Equal(t, "   1| package a\n   2| \n   3| func F() {}", got)
}

func TestReadReturnsAPartAndWhereToContinue(t *testing.T) {
	var b strings.Builder
	for i := 1; i <= 10; i++ {
		fmt.Fprintf(&b, "line %d\n", i)
	}
	f := file("big.txt", b.String())

	first := Read(f, 1, 4)
	assert.Equal(t, 10, first.TotalLines)
	assert.Equal(t, 1, first.StartLine)
	assert.Equal(t, 4, first.EndLine)
	assert.Equal(t, 5, first.NextStartLine)
	assert.Equal(t, "   1| line 1\n   2| line 2\n   3| line 3\n   4| line 4", first.Content)

	last := Read(f, first.NextStartLine+5, 4)
	assert.Equal(t, 10, last.StartLine)
	assert.Equal(t, 10, last.EndLine)
	assert.Zero(t, last.NextStartLine, "the end of the file was reached")

	beyond := Read(f, 11, 4)
	assert.Equal(t, 10, beyond.TotalLines)
	assert.Equal(t, 10, beyond.EndLine, "EndLine is StartLine-1 when nothing is left")
	assert.Empty(t, beyond.Content)
	assert.Zero(t, beyond.NextStartLine)

	assert.Equal(t, 1, Read(f, -5, 1).StartLine, "a start before the first line means the first line")
	assert.Equal(t, 10, Read(f, 1, 0).EndLine, "0 lines means the default, which here is the whole file")
	assert.Equal(t, 10, Read(f, 1, 100000).EndLine, "the limit is applied")
}

func TestReadIsCappedByLinesAndBySize(t *testing.T) {
	many := strings.Repeat("x\n", 1000)
	assert.Equal(t, MaxReadLines, Read(file("many", many), 1, 100000).EndLine, "never more than MaxReadLines")

	wide := strings.Repeat(strings.Repeat("y", 500)+"\n", 100)
	got := Read(file("wide", wide), 1, MaxReadLines)
	assert.LessOrEqual(t, len(got.Content), MaxReadBytes)
	assert.Greater(t, got.EndLine, 1, "more than one line fits")
	assert.Equal(t, got.EndLine+1, got.NextStartLine, "the next reading starts where this one stopped")
}

func TestAVeryLongLineIsCutAndDoesNotTakeTheWholeWindow(t *testing.T) {
	// A minified file is one line of 50 000 characters.
	minified := strings.Repeat("z", 50000) + "\nsecond\n"

	got := Read(file("min.js", minified), 1, 10)

	assert.Contains(t, got.Content, "[line cut]")
	assert.Less(t, len(got.Content), MaxLineChars+100, "the cut line plus the short one after it")
	assert.Equal(t, 2, got.EndLine, "the line after it still fits")
	assert.Contains(t, Inline(file("min.js", minified)), "[line cut]", "the prompt cuts it too")

	// Several long lines: only as many as fit in MaxReadBytes, and the reading goes on from there.
	long := strings.Repeat(strings.Repeat("q", 5000)+"\n", 10)
	part := Read(file("long.txt", long), 1, 10)
	assert.LessOrEqual(t, len(part.Content), MaxReadBytes)
	assert.Equal(t, part.EndLine+1, part.NextStartLine)
	assert.GreaterOrEqual(t, part.EndLine, 1)
}

// ------------------------------------------------------------------ with the database
type env struct {
	svc    *Service
	db     *db.Database
	ctx    context.Context
	userID uuid.UUID
}

func newEnv(t *testing.T) *env {
	t.Helper()
	connString := dbtest.ConnString(t)
	t.Chdir("../..") // db.RunMigrations resolves "migrations" from the working directory

	ctx := context.Background()
	require.NoError(t, db.RunMigrations(connString))
	database, err := db.Connect(ctx, connString)
	require.NoError(t, err)
	t.Cleanup(database.Close)

	e := &env{svc: NewService(database), db: database, ctx: ctx}
	e.userID = e.newUser(t)
	return e
}

func (e *env) newUser(t *testing.T) uuid.UUID {
	t.Helper()
	user, err := e.db.Queries.CreateUser(e.ctx)
	require.NoError(t, err)
	t.Cleanup(func() { dbtest.DeleteUser(t, e.db.Pool, user.ID) })
	return user.ID
}

func TestAddStoresTheFileAndTheListNeverReadsIt(t *testing.T) {
	e := newEnv(t)
	code := "package main\n\nfunc main() {}\n"

	added, err := e.svc.Add(e.ctx, e.userID, "s1", "main.go", code)

	require.NoError(t, err)
	assert.True(t, added.Created)
	assert.False(t, added.Replaced)
	assert.False(t, added.Unchanged)
	assert.Equal(t, "main.go", added.Name)
	assert.Equal(t, len(code), added.Bytes)
	assert.Equal(t, 3, added.Lines)
	assert.Equal(t, EstimateTokens(len(code)), added.Tokens)
	assert.Equal(t, 1, added.Version)

	list, err := e.svc.List(e.ctx, e.userID, "s1")
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, added.Info.ID, list[0].ID)

	files, err := e.svc.Files(e.ctx, e.userID, "s1")
	require.NoError(t, err)
	require.Len(t, files, 1)
	assert.Equal(t, code, files[0].Content)
}

func TestAttachingTheSameNameIsANewVersionAndTheSameContentChangesNothing(t *testing.T) {
	e := newEnv(t)
	first, err := e.svc.Add(e.ctx, e.userID, "s1", "a.py", "x = 1\n")
	require.NoError(t, err)

	again, err := e.svc.Add(e.ctx, e.userID, "s1", "a.py", "x = 1\n")
	require.NoError(t, err)
	assert.True(t, again.Unchanged)
	assert.False(t, again.Created)
	assert.Equal(t, first.Info.ID, again.Info.ID)
	assert.Equal(t, 1, again.Version, "nothing changed, so it is still version 1")

	edited, err := e.svc.Add(e.ctx, e.userID, "s1", "a.py", "x = 2\ny = 3\n")
	require.NoError(t, err)
	assert.True(t, edited.Replaced)
	assert.Equal(t, 2, edited.Version)
	assert.Equal(t, 2, edited.Lines)
	assert.Equal(t, first.Info.ID, edited.Info.ID, "the same file, a newer version")

	list, err := e.svc.List(e.ctx, e.userID, "s1")
	require.NoError(t, err)
	assert.Len(t, list, 1)
	found, err := e.svc.Find(e.ctx, e.userID, "s1", "a.py")
	require.NoError(t, err)
	assert.Equal(t, "x = 2\ny = 3\n", found.Content, "the model reads the new version")
}

func TestWhatIsStoredIsNormalized(t *testing.T) {
	e := newEnv(t)

	added, err := e.svc.Add(e.ctx, e.userID, "s1", `C:\work\src\run.ps1`, "\xef\xbb\xbfa\r\nb\r\n")

	require.NoError(t, err)
	assert.Equal(t, "run.ps1", added.Name)
	assert.Equal(t, 2, added.Lines)
	found, err := e.svc.Find(e.ctx, e.userID, "s1", "run.ps1")
	require.NoError(t, err)
	assert.Equal(t, "a\nb\n", found.Content)
}

func TestWhatIsNotTextOrNotAFileIsRefused(t *testing.T) {
	e := newEnv(t)
	for name, tc := range map[string]struct {
		session, name, content string
		want                   error
	}{
		"empty":            {"s", "a.txt", "  \n", ErrEmpty},
		"binary":           {"s", "a.bin", "MZ\x00\x90", ErrBinary},
		"too large":        {"s", "a.txt", strings.Repeat("a", MaxFileBytes+1), ErrTooLarge},
		"no name":          {"s", "..", "x", ErrBadName},
		"no session":       {"", "a.txt", "x", ErrBadSession},
		"control in sess.": {"a\nb", "a.txt", "x", ErrBadSession},
		"long session":     {strings.Repeat("s", 129), "a.txt", "x", ErrBadSession},
	} {
		_, err := e.svc.Add(e.ctx, e.userID, tc.session, tc.name, tc.content)
		assert.ErrorIs(t, err, tc.want, name)
	}

	list, err := e.svc.List(e.ctx, e.userID, "s")
	require.NoError(t, err)
	assert.Empty(t, list, "nothing was stored")
}

func TestAConversationHasLimits(t *testing.T) {
	e := newEnv(t)
	for i := 0; i < MaxSessionFiles; i++ {
		_, err := e.svc.Add(e.ctx, e.userID, "s", fmt.Sprintf("f%02d.txt", i), "x")
		require.NoError(t, err)
	}

	_, err := e.svc.Add(e.ctx, e.userID, "s", "one-too-many.txt", "x")
	assert.ErrorIs(t, err, ErrTooMany)
	_, err = e.svc.Add(e.ctx, e.userID, "s", "f00.txt", "a new version of an old file")
	assert.NoError(t, err, "a new version is not a new file")
	_, err = e.svc.Add(e.ctx, e.userID, "other-session", "one-too-many.txt", "x")
	assert.NoError(t, err, "the limit is per conversation")
}

func TestTheFilesOfAConversationHaveATotalSize(t *testing.T) {
	e := newEnv(t)
	big := strings.Repeat("a", 200_000)
	for i := 0; i < 5; i++ {
		_, err := e.svc.Add(e.ctx, e.userID, "s", fmt.Sprintf("big%d.txt", i), big)
		require.NoError(t, err)
	}

	_, err := e.svc.Add(e.ctx, e.userID, "s", "big5.txt", big)
	assert.ErrorIs(t, err, ErrSessionFull)

	_, err = e.svc.Add(e.ctx, e.userID, "s", "big0.txt", "small now")
	require.NoError(t, err, "a smaller version of a file makes room")
	_, err = e.svc.Add(e.ctx, e.userID, "s", "big5.txt", big)
	assert.NoError(t, err)
}

func TestFilesBelongToTheirOwner(t *testing.T) {
	e := newEnv(t)
	stranger := e.newUser(t)
	mine, err := e.svc.Add(e.ctx, e.userID, "shared-id", "secret.py", "API_KEY = 'x'\n")
	require.NoError(t, err)

	list, err := e.svc.List(e.ctx, stranger, "shared-id")
	require.NoError(t, err)
	assert.Empty(t, list, "the same session id of another user is another conversation")
	_, err = e.svc.Find(e.ctx, stranger, "shared-id", "secret.py")
	assert.ErrorIs(t, err, ErrNotFound)
	files, err := e.svc.Files(e.ctx, stranger, "shared-id")
	require.NoError(t, err)
	assert.Empty(t, files)

	assert.ErrorIs(t, e.svc.Delete(e.ctx, stranger, mine.Info.ID), ErrNotFound, "someone else's id")
	still, err := e.svc.Find(e.ctx, e.userID, "shared-id", "secret.py")
	require.NoError(t, err)
	assert.Equal(t, mine.Info.ID, still.ID)

	// The same name in the stranger's session is a separate file.
	_, err = e.svc.Add(e.ctx, stranger, "shared-id", "secret.py", "other")
	require.NoError(t, err)
	still, err = e.svc.Find(e.ctx, e.userID, "shared-id", "secret.py")
	require.NoError(t, err)
	assert.Equal(t, "API_KEY = 'x'\n", still.Content)
}

func TestDeleteRemovesTheFile(t *testing.T) {
	e := newEnv(t)
	added, err := e.svc.Add(e.ctx, e.userID, "s", "a.txt", "x")
	require.NoError(t, err)

	require.NoError(t, e.svc.Delete(e.ctx, e.userID, added.Info.ID))

	assert.ErrorIs(t, e.svc.Delete(e.ctx, e.userID, added.Info.ID), ErrNotFound, "twice")
	_, err = e.svc.Find(e.ctx, e.userID, "s", "a.txt")
	assert.ErrorIs(t, err, ErrNotFound)
}

func TestFindToleratesLetterCaseButNotAmbiguity(t *testing.T) {
	e := newEnv(t)
	_, err := e.svc.Add(e.ctx, e.userID, "s", "Main.go", "package main\n")
	require.NoError(t, err)

	f, err := e.svc.Find(e.ctx, e.userID, "s", "main.go")
	require.NoError(t, err)
	assert.Equal(t, "Main.go", f.Name)
	_, err = e.svc.Find(e.ctx, e.userID, "s", "other.go")
	assert.ErrorIs(t, err, ErrNotFound)

	_, err = e.svc.Add(e.ctx, e.userID, "s", "MAIN.go", "package other\n")
	require.NoError(t, err)
	exact, err := e.svc.Find(e.ctx, e.userID, "s", "Main.go")
	require.NoError(t, err)
	assert.Equal(t, "package main\n", exact.Content, "an exact name always wins")
	_, err = e.svc.Find(e.ctx, e.userID, "s", "main.GO")
	assert.ErrorIs(t, err, ErrAmbiguous)
}

func TestOldFilesAreRemovedWhenSomethingNewIsAttached(t *testing.T) {
	e := newEnv(t)
	_, err := e.svc.Add(e.ctx, e.userID, "old-chat", "old.txt", "x")
	require.NoError(t, err)

	later := NewService(e.db)
	later.now = func() time.Time { return time.Now().Add(StaleAfter + 24*time.Hour) }
	_, err = later.Add(e.ctx, e.userID, "new-chat", "new.txt", "y")
	require.NoError(t, err)

	old, err := e.svc.List(e.ctx, e.userID, "old-chat")
	require.NoError(t, err)
	assert.Empty(t, old, "a month without being attached again")
	fresh, err := e.svc.List(e.ctx, e.userID, "new-chat")
	require.NoError(t, err)
	assert.Len(t, fresh, 1)
}
