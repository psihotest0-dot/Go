package logfile

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	return string(data)
}

func TestTruncatingAppendsUnderTheLimit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metrics.log")
	w := NewTruncating(path, 100)
	defer w.Close()

	for _, line := range []string{"a\n", "b\n", "c\n"} {
		_, err := w.Write([]byte(line))
		require.NoError(t, err)
	}

	assert.Equal(t, "a\nb\nc\n", readFile(t, path))
}

func TestTruncatingStartsOverAtTheLimit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metrics.log")
	w := NewTruncating(path, 10)
	defer w.Close()

	_, err := w.Write([]byte("12345\n")) // 6 bytes
	require.NoError(t, err)
	_, err = w.Write([]byte("abcd\n")) // 5 more would be 11 > 10
	require.NoError(t, err)

	assert.Equal(t, "abcd\n", readFile(t, path), "the old line is deleted, the new one is kept whole")

	// A file exactly at the limit still takes the line that fills it.
	_, err = w.Write([]byte("efghi")) // 5 + 5 = 10, not over
	require.NoError(t, err)
	assert.Equal(t, "abcd\nefghi", readFile(t, path))
}

func TestTruncatingNeverGrowsPastTheLimit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metrics.log")
	w := NewTruncating(path, 1000)
	defer w.Close()

	line := []byte(strings.Repeat("x", 99) + "\n")
	for i := 0; i < 500; i++ {
		_, err := w.Write(line)
		require.NoError(t, err)
		info, err := os.Stat(path)
		require.NoError(t, err)
		require.LessOrEqual(t, info.Size(), int64(1000), "after write %d", i)
	}
}

// A line longer than the limit is written whole, in an otherwise empty file.
func TestTruncatingNeverCutsALine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metrics.log")
	w := NewTruncating(path, 5)
	defer w.Close()

	_, err := w.Write([]byte("ab\n"))
	require.NoError(t, err)
	_, err = w.Write([]byte("0123456789\n"))
	require.NoError(t, err)

	assert.Equal(t, "0123456789\n", readFile(t, path))
}

// After a restart the file may already be over the limit (the limit was lowered,
// or the process died): the first write empties it instead of appending.
func TestTruncatingEmptiesAFileThatIsAlreadyTooBig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metrics.log")
	require.NoError(t, os.WriteFile(path, []byte(strings.Repeat("old\n", 100)), 0o644))
	w := NewTruncating(path, 50)
	defer w.Close()

	_, err := w.Write([]byte("new\n"))
	require.NoError(t, err)

	assert.Equal(t, "new\n", readFile(t, path))
}

// After a restart a small file is continued, not wiped.
func TestTruncatingContinuesASmallExistingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metrics.log")
	require.NoError(t, os.WriteFile(path, []byte("old\n"), 0o644))
	w := NewTruncating(path, 50)
	defer w.Close()

	_, err := w.Write([]byte("new\n"))
	require.NoError(t, err)

	assert.Equal(t, "old\nnew\n", readFile(t, path))
}

func TestTruncatingReopensAfterClose(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metrics.log")
	w := NewTruncating(path, 50)

	_, err := w.Write([]byte("one\n"))
	require.NoError(t, err)
	require.NoError(t, w.Close())
	require.NoError(t, w.Close(), "closing twice is harmless")
	_, err = w.Write([]byte("two\n"))
	require.NoError(t, err)
	require.NoError(t, w.Close())

	assert.Equal(t, "one\ntwo\n", readFile(t, path))
}

func TestTruncatingReportsAnUnopenableFile(t *testing.T) {
	w := NewTruncating(filepath.Join(t.TempDir(), "no-such-dir", "metrics.log"), 50)

	_, err := w.Write([]byte("x\n"))

	assert.Error(t, err)
}

func TestTruncatingIsSafeForConcurrentWrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metrics.log")
	w := NewTruncating(path, 400)
	defer w.Close()

	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				_, err := w.Write([]byte(strings.Repeat("y", 39) + "\n"))
				assert.NoError(t, err)
			}
		}()
	}
	wg.Wait()

	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.LessOrEqual(t, info.Size(), int64(400))
	for _, line := range strings.Split(strings.TrimSuffix(readFile(t, path), "\n"), "\n") {
		assert.Equal(t, strings.Repeat("y", 39), line, "no line may be cut or mixed")
	}
}
