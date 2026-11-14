package userfiles

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var (
	alice = uuid.MustParse("11111111-1111-1111-1111-111111111111")
	bob   = uuid.MustParse("22222222-2222-2222-2222-222222222222")
)

// newStore opens a store in a fresh folder and returns it with the folder.
func newStore(t *testing.T, limits Limits) (*Store, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "files")
	s, err := Open(dir, limits)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	return s, dir
}

// tree lists every path under dir, relative and sorted: what the disk holds.
func tree(t *testing.T, dir string) []string {
	t.Helper()
	var paths []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		if rel != "." {
			paths = append(paths, filepath.ToSlash(rel))
		}
		return nil
	})
	require.NoError(t, err)
	sort.Strings(paths)
	return paths
}

func userDir(dir string, id uuid.UUID) string { return filepath.Join(dir, id.String()) }

func names(infos []Info) []string {
	out := make([]string, len(infos))
	for i, f := range infos {
		out[i] = f.Name
	}
	return out
}

func TestOpenNeedsAnAbsoluteAndShortFolder(t *testing.T) {
	_, err := Open("relative/folder", Limits{})
	assert.Error(t, err)

	_, err = Open(filepath.Join(t.TempDir(), strings.Repeat("d", MaxFolderLen)), Limits{})
	assert.Error(t, err)

	s, dir := newStore(t, Limits{})
	info, err := os.Stat(dir)
	require.NoError(t, err)
	assert.True(t, info.IsDir())
	assert.Equal(t, DefaultLimits(), s.Limits(), "zero limits take the defaults")
}

func TestCreateReadAndListRoundTrip(t *testing.T) {
	s, dir := newStore(t, Limits{})

	info, err := s.Create(alice, "списък за пазаруване.txt", "мляко\nхляб\nяйца\n")
	require.NoError(t, err)
	assert.Equal(t, "списък за пазаруване.txt", info.Name)
	assert.EqualValues(t, len("мляко\nхляб\nяйца\n"), info.Size)

	part, err := s.Read(alice, "списък за пазаруване.txt", 1, 10)
	require.NoError(t, err)
	assert.Equal(t, "мляко\nхляб\nяйца", part.Content)
	assert.Equal(t, 3, part.TotalLines)
	assert.Equal(t, 1, part.StartLine)
	assert.Equal(t, 3, part.EndLine)
	assert.Zero(t, part.NextStartLine)

	list, err := s.List(alice)
	require.NoError(t, err)
	assert.Equal(t, []string{"списък за пазаруване.txt"}, names(list))

	assert.Equal(t, []string{alice.String(), alice.String() + "/списък за пазаруване.txt"}, tree(t, dir))
}

func TestCreateNeverReplacesAFile(t *testing.T) {
	s, _ := newStore(t, Limits{})
	_, err := s.Create(alice, "a.txt", "first")
	require.NoError(t, err)

	_, err = s.Create(alice, "a.txt", "second")

	require.ErrorIs(t, err, ErrExists)
	part, err := s.Read(alice, "a.txt", 1, 10)
	require.NoError(t, err)
	assert.Equal(t, "first", part.Content, "the file must be as it was")
}

func TestUsersDoNotSeeEachOther(t *testing.T) {
	s, dir := newStore(t, Limits{})
	_, err := s.Create(alice, "a.txt", "alice's text")
	require.NoError(t, err)

	list, err := s.List(bob)
	require.NoError(t, err)
	assert.Empty(t, list)
	_, err = s.Read(bob, "a.txt", 1, 10)
	require.ErrorIs(t, err, ErrNotFound)
	_, err = s.Replace(bob, "a.txt", "bob's text")
	require.ErrorIs(t, err, ErrNotFound)
	_, err = s.Append(bob, "a.txt", "bob's line")
	require.ErrorIs(t, err, ErrNotFound)

	_, err = s.Create(bob, "a.txt", "bob's text")
	require.NoError(t, err)
	part, err := s.Read(alice, "a.txt", 1, 10)
	require.NoError(t, err)
	assert.Equal(t, "alice's text", part.Content)

	_, err = s.List(uuid.Nil)
	assert.Error(t, err, "there is no anonymous folder")
	assert.Equal(t, []string{alice.String(), alice.String() + "/a.txt", bob.String(), bob.String() + "/a.txt"}, tree(t, dir))
}

// A bad name stops before the disk, in every call, and nothing outside the user's own
// folder is touched.
func TestHostileNamesNeverReachTheDisk(t *testing.T) {
	s, dir := newStore(t, Limits{})
	outside := filepath.Join(filepath.Dir(dir), "secret.txt")
	require.NoError(t, os.WriteFile(outside, []byte("do not touch"), 0o600))
	_, err := s.Create(alice, "ok.txt", "fine")
	require.NoError(t, err)
	before := tree(t, filepath.Dir(dir))

	for _, name := range []string{
		"../secret.txt",
		`..\secret.txt`,
		"../../secret.txt",
		outside,
		"/etc/passwd.txt",
		`C:\Windows\x.txt`,
		"ok.txt:stream",
		"CON.txt",
		"NUL",
		"ok.txt.",
		"ok.exe",
		"PROGRA~1.txt",
		"sub/ok.txt",
		"",
	} {
		var invalid *InvalidError
		_, err := s.Create(alice, name, "text")
		require.ErrorAs(t, err, &invalid, "Create %q", name)
		_, err = s.Replace(alice, name, "text")
		require.ErrorAs(t, err, &invalid, "Replace %q", name)
		_, err = s.Append(alice, name, "text")
		require.ErrorAs(t, err, &invalid, "Append %q", name)
		_, err = s.Read(alice, name, 1, 10)
		require.ErrorAs(t, err, &invalid, "Read %q", name)
		_, err = s.Previous(alice, name)
		require.ErrorAs(t, err, &invalid, "Previous %q", name)
	}

	assert.Equal(t, before, tree(t, filepath.Dir(dir)), "not a file was made, renamed or removed")
	data, err := os.ReadFile(outside)
	require.NoError(t, err)
	assert.Equal(t, "do not touch", string(data))
}

func TestContentIsChecked(t *testing.T) {
	s, _ := newStore(t, Limits{MaxFileBytes: 100})
	bad := map[string]string{
		"empty":                "",
		"only spaces":          "  \n\t ",
		"NUL":                  "a\x00b",
		"escape":               "a\x1b[31mb",
		"delete":               "a\x7fb",
		"not UTF-8":            "a\xffb",
		"over the file limit":  strings.Repeat("x", 101),
		"form feed":            "a\fb",
		"vertical tab":         "a\vb",
		"bell":                 "a\ab",
		"carriage return only": "",
	}
	for why, content := range bad {
		var invalid *InvalidError
		_, err := s.Create(alice, "a.txt", content)
		require.ErrorAs(t, err, &invalid, why)
	}

	_, err := s.Create(alice, "ok.txt", "tabs\tand\r\nlines\n и кирилица")
	require.NoError(t, err)
	_, err = s.Create(alice, "edge.txt", strings.Repeat("x", 100))
	require.NoError(t, err, "exactly the limit is allowed")
}

func TestLimits(t *testing.T) {
	t.Run("number of files", func(t *testing.T) {
		s, _ := newStore(t, Limits{MaxFiles: 3})
		for i := 0; i < 3; i++ {
			_, err := s.Create(alice, fmt.Sprintf("f%d.txt", i), "x")
			require.NoError(t, err)
		}
		_, err := s.Create(alice, "f3.txt", "x")
		require.ErrorIs(t, err, ErrTooManyFiles)

		_, err = s.Create(bob, "f0.txt", "x")
		require.NoError(t, err, "the limit is for each user")
		_, err = s.Replace(alice, "f0.txt", "y")
		require.NoError(t, err, "replacing is not a new file")
	})

	t.Run("total size", func(t *testing.T) {
		s, _ := newStore(t, Limits{MaxTotalBytes: 100})
		_, err := s.Create(alice, "a.txt", strings.Repeat("x", 60))
		require.NoError(t, err)
		_, err = s.Create(alice, "b.txt", strings.Repeat("x", 41))
		require.ErrorIs(t, err, ErrQuotaExceeded)
		_, err = s.Create(alice, "b.txt", strings.Repeat("x", 40))
		require.NoError(t, err)

		_, err = s.Append(alice, "a.txt", "y")
		require.ErrorIs(t, err, ErrQuotaExceeded)
	})

	t.Run("previous versions count", func(t *testing.T) {
		s, _ := newStore(t, Limits{MaxTotalBytes: 100})
		_, err := s.Create(alice, "a.txt", strings.Repeat("x", 50))
		require.NoError(t, err)
		_, err = s.Replace(alice, "a.txt", strings.Repeat("y", 50))
		require.NoError(t, err, "the old text is kept, so 100 bytes are used")
		_, err = s.Create(alice, "b.txt", "z")
		require.ErrorIs(t, err, ErrQuotaExceeded, "the previous version takes room too")
	})

	t.Run("a file grows past its limit by appending", func(t *testing.T) {
		s, _ := newStore(t, Limits{MaxFileBytes: 20})
		_, err := s.Create(alice, "a.txt", strings.Repeat("x", 15))
		require.NoError(t, err)
		var invalid *InvalidError
		_, err = s.Append(alice, "a.txt", strings.Repeat("y", 10))
		require.ErrorAs(t, err, &invalid)
		part, err := s.Read(alice, "a.txt", 1, 10)
		require.NoError(t, err)
		assert.Equal(t, strings.Repeat("x", 15), part.Content, "a refused append changes nothing")
	})
}

func TestReplaceKeepsThePreviousVersion(t *testing.T) {
	s, dir := newStore(t, Limits{})

	_, err := s.Replace(alice, "a.txt", "new")
	require.ErrorIs(t, err, ErrNotFound, "Replace is for a file that exists")

	_, err = s.Create(alice, "a.txt", "one")
	require.NoError(t, err)
	_, err = s.Replace(alice, "a.txt", "two")
	require.NoError(t, err)
	_, err = s.Replace(alice, "a.txt", "three")
	require.NoError(t, err)

	part, err := s.Read(alice, "a.txt", 1, 10)
	require.NoError(t, err)
	assert.Equal(t, "three", part.Content)
	prev, err := s.Previous(alice, "a.txt")
	require.NoError(t, err)
	assert.Equal(t, "two", prev, "one generation is kept")

	_, err = s.Previous(alice, "other.txt")
	require.ErrorIs(t, err, ErrNotFound)

	list, err := s.List(alice)
	require.NoError(t, err)
	assert.Equal(t, []string{"a.txt"}, names(list), "the previous version is not a file of the user")
	for _, p := range tree(t, userDir(dir, alice)) {
		assert.False(t, strings.HasPrefix(p, tempPrefix), "no temporary file is left: %s", p)
	}
}

// If the old version cannot be kept, the file must stay as it is: a Replace that loses the
// old text is the one thing it must not do.
func TestReplaceDoesNothingWhenThePreviousVersionCannotBeKept(t *testing.T) {
	s, dir := newStore(t, Limits{})
	_, err := s.Create(alice, "a.txt", "precious")
	require.NoError(t, err)
	// A file where the folder of previous versions should be.
	require.NoError(t, os.WriteFile(filepath.Join(userDir(dir, alice), previousDir), []byte("in the way"), 0o600))

	_, err = s.Replace(alice, "a.txt", "replacement")

	require.Error(t, err)
	part, err := s.Read(alice, "a.txt", 1, 10)
	require.NoError(t, err)
	assert.Equal(t, "precious", part.Content)
	for _, p := range tree(t, userDir(dir, alice)) {
		assert.False(t, strings.HasPrefix(p, tempPrefix), "no temporary file is left: %s", p)
	}
}

func TestAppendKeepsOneEntryToALine(t *testing.T) {
	s, _ := newStore(t, Limits{})
	_, err := s.Create(alice, "список.txt", "мляко")
	require.NoError(t, err)

	_, err = s.Append(alice, "список.txt", "хляб")
	require.NoError(t, err)
	_, err = s.Append(alice, "список.txt", "сирене\n")
	require.NoError(t, err)
	_, err = s.Append(alice, "список.txt", "яйца")
	require.NoError(t, err)

	part, err := s.Read(alice, "список.txt", 1, 10)
	require.NoError(t, err)
	assert.Equal(t, "мляко\nхляб\nсирене\nяйца", part.Content)
	assert.Equal(t, 4, part.TotalLines)

	_, err = s.Append(alice, "missing.txt", "x")
	require.ErrorIs(t, err, ErrNotFound)
	var invalid *InvalidError
	_, err = s.Append(alice, "список.txt", "  ")
	require.ErrorAs(t, err, &invalid)
}

func TestReadInParts(t *testing.T) {
	s, _ := newStore(t, Limits{})
	var lines []string
	for i := 1; i <= 10; i++ {
		lines = append(lines, fmt.Sprintf("line %d", i))
	}
	_, err := s.Create(alice, "a.txt", strings.Join(lines, "\n")+"\n")
	require.NoError(t, err)

	part, err := s.Read(alice, "a.txt", 1, 4)
	require.NoError(t, err)
	assert.Equal(t, "line 1\nline 2\nline 3\nline 4", part.Content)
	assert.Equal(t, 1, part.StartLine)
	assert.Equal(t, 4, part.EndLine)
	assert.Equal(t, 5, part.NextStartLine)
	assert.Equal(t, 10, part.TotalLines)

	part, err = s.Read(alice, "a.txt", part.NextStartLine, 4)
	require.NoError(t, err)
	assert.Equal(t, "line 5\nline 6\nline 7\nline 8", part.Content)

	part, err = s.Read(alice, "a.txt", part.NextStartLine, 4)
	require.NoError(t, err)
	assert.Equal(t, "line 9\nline 10", part.Content)
	assert.Zero(t, part.NextStartLine, "the end of the file")

	part, err = s.Read(alice, "a.txt", 11, 4)
	require.NoError(t, err)
	assert.Empty(t, part.Content)
	assert.Equal(t, 10, part.EndLine)
	assert.Zero(t, part.NextStartLine)

	part, err = s.Read(alice, "a.txt", 0, 0)
	require.NoError(t, err)
	assert.Equal(t, 1, part.StartLine, "no start means the first line")
	assert.Equal(t, 10, part.EndLine, "no count means the default")

	_, err = s.Read(alice, "nothing.txt", 1, 10)
	require.ErrorIs(t, err, ErrNotFound)
}

func TestReadStaysWithinItsByteLimit(t *testing.T) {
	s, _ := newStore(t, Limits{MaxReadBytes: 25})
	_, err := s.Create(alice, "a.txt", "0123456789\n0123456789\n0123456789\n0123456789\n")
	require.NoError(t, err)

	part, err := s.Read(alice, "a.txt", 1, 100)
	require.NoError(t, err)
	assert.Equal(t, "0123456789\n0123456789", part.Content, "whole lines only, 21 bytes of 25")
	assert.Equal(t, 2, part.EndLine)
	assert.Equal(t, 3, part.NextStartLine)

	// One line longer than the limit is cut, and only at a character boundary.
	_, err = s.Create(alice, "long.txt", strings.Repeat("ъ", 40)+"\nnext\n")
	require.NoError(t, err)
	part, err = s.Read(alice, "long.txt", 1, 100)
	require.NoError(t, err)
	assert.True(t, part.Truncated)
	assert.LessOrEqual(t, len(part.Content), 25)
	assert.Equal(t, strings.Repeat("ъ", len(part.Content)/2), part.Content, "no half of a character")
	assert.Equal(t, 1, part.EndLine)
	assert.Equal(t, 2, part.NextStartLine)
}

func TestReadFilesTheOwnerPutThere(t *testing.T) {
	s, dir := newStore(t, Limits{MaxFileBytes: 100})
	_, err := s.Create(alice, "seed.txt", "x")
	require.NoError(t, err)
	udir := userDir(dir, alice)

	require.NoError(t, os.WriteFile(filepath.Join(udir, "windows.txt"), []byte("\xef\xbb\xbfone\r\ntwo\r\n"), 0o600))
	part, err := s.Read(alice, "windows.txt", 1, 10)
	require.NoError(t, err)
	assert.Equal(t, "one\ntwo", part.Content, "no byte order mark and no carriage returns")

	require.NoError(t, os.WriteFile(filepath.Join(udir, "binary.txt"), []byte("ab\xff\xfecd"), 0o600))
	var invalid *InvalidError
	_, err = s.Read(alice, "binary.txt", 1, 10)
	require.ErrorAs(t, err, &invalid)

	require.NoError(t, os.WriteFile(filepath.Join(udir, "huge.txt"), []byte(strings.Repeat("x", 401)), 0o600))
	_, err = s.Read(alice, "huge.txt", 1, 10)
	require.ErrorAs(t, err, &invalid, "more than four times what the model may write")
}

func TestOnlyUsableFilesAreListed(t *testing.T) {
	s, dir := newStore(t, Limits{})
	_, err := s.Create(alice, "b.txt", "x")
	require.NoError(t, err)
	_, err = s.Create(alice, "a.md", "x")
	require.NoError(t, err)
	udir := userDir(dir, alice)
	require.NoError(t, os.WriteFile(filepath.Join(udir, "photo.jpg"), []byte("x"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(udir, "run.ps1"), []byte("x"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(udir, tempPrefix+"abc"), []byte("x"), 0o600))
	require.NoError(t, os.Mkdir(filepath.Join(udir, "folder.txt"), 0o700))
	require.NoError(t, os.Mkdir(filepath.Join(udir, previousDir), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(udir, previousDir, "c.txt"), []byte("x"), 0o600))

	list, err := s.List(alice)
	require.NoError(t, err)

	assert.Equal(t, []string{"a.md", "b.txt"}, names(list))

	var invalid *InvalidError
	_, err = s.Read(alice, "photo.jpg", 1, 10)
	require.ErrorAs(t, err, &invalid)
	_, err = s.Read(alice, "folder.txt", 1, 10)
	require.ErrorIs(t, err, ErrNotFound, "a folder is not a file")
	_, err = s.Append(alice, "folder.txt", "x")
	require.Error(t, err)
	_, err = s.Replace(alice, "folder.txt", "x")
	require.Error(t, err)
}

// A link in the folder must not lead the model to something outside it. (Linux; on
// Windows a symbolic link needs a privilege the tests may not have, and a junction has
// its own test.)
func TestLinksCannotLeadOutOfTheFolder(t *testing.T) {
	s, dir := newStore(t, Limits{})
	outside := filepath.Dir(dir)
	secret := filepath.Join(outside, "secret.txt")
	require.NoError(t, os.WriteFile(secret, []byte("do not touch"), 0o600))
	_, err := s.Create(alice, "seed.txt", "x")
	require.NoError(t, err)
	udir := userDir(dir, alice)

	if err := os.Symlink(filepath.Join("..", "..", "secret.txt"), filepath.Join(udir, "relative.txt")); err != nil {
		t.Skipf("cannot make a symbolic link here: %v", err)
	}
	require.NoError(t, os.Symlink(secret, filepath.Join(udir, "absolute.txt")))
	outsideDir := filepath.Join(outside, "elsewhere")
	require.NoError(t, os.Mkdir(outsideDir, 0o700))
	require.NoError(t, os.Symlink(outsideDir, filepath.Join(udir, previousDir)))

	for _, name := range []string{"relative.txt", "absolute.txt"} {
		_, err = s.Read(alice, name, 1, 10)
		require.Error(t, err, "Read %s", name)
		_, err = s.Replace(alice, name, "overwritten")
		require.Error(t, err, "Replace %s", name)
		_, err = s.Append(alice, name, "added")
		require.Error(t, err, "Append %s", name)
		_, err = s.Create(alice, name, "created")
		require.ErrorIs(t, err, ErrExists, "Create %s", name)
	}
	// Replace of a real file whose folder of previous versions is a link going out.
	_, err = s.Replace(alice, "seed.txt", "y")
	require.Error(t, err)

	data, err := os.ReadFile(secret)
	require.NoError(t, err)
	assert.Equal(t, "do not touch", string(data))
	entries, err := os.ReadDir(outsideDir)
	require.NoError(t, err)
	assert.Empty(t, entries, "nothing was written outside")
	list, err := s.List(alice)
	require.NoError(t, err)
	assert.Equal(t, []string{"seed.txt"}, names(list), "links are not listed")
	part, err := s.Read(alice, "seed.txt", 1, 10)
	require.NoError(t, err)
	assert.Equal(t, "x", part.Content, "the file the Replace refused is as it was")
}

// A junction (a Windows directory link that needs no privilege) in the place of the
// folder of previous versions or of the user's own folder must not take a write out of the
// folder.
func TestJunctionsCannotLeadOutOfTheFolder(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("junctions are a Windows thing")
	}
	s, dir := newStore(t, Limits{})
	outside := filepath.Join(filepath.Dir(dir), "elsewhere")
	require.NoError(t, os.Mkdir(outside, 0o700))
	_, err := s.Create(alice, "seed.txt", "x")
	require.NoError(t, err)

	junction := filepath.Join(userDir(dir, alice), previousDir)
	out, err := exec.Command("cmd", "/c", "mklink", "/J", junction, outside).CombinedOutput()
	require.NoError(t, err, string(out))

	_, err = s.Replace(alice, "seed.txt", "y")

	require.Error(t, err, "the previous version must not be written through a junction")
	t.Logf("Replace through a junction fails with: %v", err)
	entries, err := os.ReadDir(outside)
	require.NoError(t, err)
	assert.Empty(t, entries, "nothing was written outside")
	part, err := s.Read(alice, "seed.txt", 1, 10)
	require.NoError(t, err)
	assert.Equal(t, "x", part.Content)

	// The user's own folder as a junction.
	other := uuid.MustParse("33333333-3333-3333-3333-333333333333")
	out, err = exec.Command("cmd", "/c", "mklink", "/J", userDir(dir, other), outside).CombinedOutput()
	require.NoError(t, err, string(out))
	_, err = s.Create(other, "a.txt", "x")
	require.Error(t, err, "the folder of a user must be a real folder")
	t.Logf("Create through a junction fails with: %v", err)
	entries, err = os.ReadDir(outside)
	require.NoError(t, err)
	assert.Empty(t, entries, "nothing was written outside")
}

func TestConcurrentCreatesRespectTheLimit(t *testing.T) {
	s, _ := newStore(t, Limits{MaxFiles: 5})
	var wg sync.WaitGroup
	var created, refused atomic.Int32
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := s.Create(alice, fmt.Sprintf("f%d.txt", i), "x")
			switch {
			case err == nil:
				created.Add(1)
			case errors.Is(err, ErrTooManyFiles):
				refused.Add(1)
			default:
				t.Errorf("unexpected error: %v", err)
			}
		}(i)
	}
	wg.Wait()

	assert.EqualValues(t, 5, created.Load())
	assert.EqualValues(t, 15, refused.Load())
	list, err := s.List(alice)
	require.NoError(t, err)
	assert.Len(t, list, 5)
}

// A reader sees all of the old text or all of the new one, never a mixture. (On Windows a
// Replace can fail while a reader has the file open; that is an error for the caller, not
// a torn file.)
func TestReplaceNeverShowsAHalfWrittenFile(t *testing.T) {
	s, _ := newStore(t, Limits{})
	a, b := strings.Repeat("A", 3000), strings.Repeat("B", 3000)
	_, err := s.Create(alice, "a.txt", a)
	require.NoError(t, err)

	stop := make(chan struct{})
	var wg sync.WaitGroup
	var replaced atomic.Int32
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			text := a
			if i%2 == 0 {
				text = b
			}
			if _, err := s.Replace(alice, "a.txt", text); err == nil {
				replaced.Add(1)
			}
		}
	}()

	for i := 0; i < 300; i++ {
		part, err := s.Read(alice, "a.txt", 1, 10)
		if err != nil {
			continue
		}
		if part.Content != a && part.Content != b {
			close(stop)
			wg.Wait()
			t.Fatalf("a torn read: %d bytes, starts %q", len(part.Content), part.Content[:min(10, len(part.Content))])
		}
	}
	close(stop)
	wg.Wait()
	assert.Positive(t, replaced.Load(), "the writer made progress")
}

func TestStaleTemporaryFilesAreSweptAway(t *testing.T) {
	s, dir := newStore(t, Limits{})
	_, err := s.Create(alice, "seed.txt", "x")
	require.NoError(t, err)
	udir := userDir(dir, alice)
	stale, fresh := filepath.Join(udir, tempPrefix+"stale"), filepath.Join(udir, tempPrefix+"fresh")
	require.NoError(t, os.WriteFile(stale, []byte("x"), 0o600))
	require.NoError(t, os.WriteFile(fresh, []byte("x"), 0o600))
	old := time.Now().Add(-time.Hour)
	require.NoError(t, os.Chtimes(stale, old, old))

	_, err = s.Create(alice, "next.txt", "x")
	require.NoError(t, err)

	_, err = os.Stat(stale)
	assert.True(t, errors.Is(err, fs.ErrNotExist), "a write that died an hour ago left this")
	_, err = os.Stat(fresh)
	assert.NoError(t, err, "a write may be going on")
}

func TestStat(t *testing.T) {
	s, dir := newStore(t, Limits{})
	_, err := s.Create(alice, "a.txt", "hello")
	require.NoError(t, err)
	require.NoError(t, os.Mkdir(filepath.Join(userDir(dir, alice), "folder.txt"), 0o700))

	info, err := s.Stat(alice, "a.txt")
	require.NoError(t, err)
	assert.Equal(t, "a.txt", info.Name)
	assert.EqualValues(t, 5, info.Size)

	_, err = s.Stat(alice, "missing.txt")
	require.ErrorIs(t, err, ErrNotFound)
	_, err = s.Stat(alice, "folder.txt")
	require.ErrorIs(t, err, ErrNotFound, "a folder is not a file")
	_, err = s.Stat(bob, "a.txt")
	require.ErrorIs(t, err, ErrNotFound, "another user's file")
	var invalid *InvalidError
	_, err = s.Stat(alice, "../a.txt")
	require.ErrorAs(t, err, &invalid)
}
