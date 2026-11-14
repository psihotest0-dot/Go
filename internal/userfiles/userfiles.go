// Package userfiles is the folder where the assistant keeps the user's own text files:
// notes, lists, drafts it was asked to write down. It is not the library of books and
// not the files attached to a conversation; those live in the database.
//
// The model chooses the names and the text, and the text it writes may have come from a
// web page or a book. So the package keeps the model on a short leash, and nothing here
// is left to the prompt:
//
//   - every user has one flat folder, <folder>/<user uuid>, opened as an os.Root: a name
//     cannot reach outside it by "..", by a link or by a race, and there are no
//     subfolders to put anything in;
//   - a name is plain (ValidateName) and ends with a harmless text extension;
//   - a file, the number of files and the total are bounded;
//   - a new file is made with O_EXCL, so it never replaces another by accident; replacing
//     is a separate call, which keeps the previous version in <user>/.previous and swaps
//     the file in through a temporary file;
//   - the model has no way to delete.
//
// The package knows nothing about models or the database. Its only HTTP is http.go: the
// routes with which the user sees, reads and clears the files; the model has tools of its
// own (in the agent) that call the same Store, and none of them deletes.
package userfiles

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

const (
	DefaultMaxFileBytes  = 64 << 10
	DefaultMaxFiles      = 200
	DefaultMaxTotalBytes = 20 << 20
	// DefaultMaxReadBytes is how much of a file one Read returns: about 800 tokens of
	// Bulgarian text, so that reading does not eat the room of the answer.
	DefaultMaxReadBytes = 4000
	// DefaultReadLines is how many lines one Read returns when the caller does not say.
	DefaultReadLines = 200

	// MaxFolderLen bounds the path of the folder itself (the user's directory and the file
	// name come after it).
	MaxFolderLen = 150

	// previousDir keeps the version a Replace took the place of. Its name cannot be a
	// file name (it starts with a dot), so no call of the model can name it.
	previousDir = ".previous"
	// tempPrefix starts the name of a file that is being written; same reason.
	tempPrefix = ".tmp-"
	// staleTempAge is how old a temporary file must be to be swept away: a write that
	// died halfway.
	staleTempAge = 10 * time.Minute

	// readCapFactor: a file the owner put in the folder by hand may be bigger than the
	// model may write; it is read up to this many times MaxFileBytes.
	readCapFactor = 4
)

var (
	// ErrNotFound: there is no such file.
	ErrNotFound = errors.New("no such file")
	// ErrExists: a file of that name is there already.
	ErrExists = errors.New("a file with this name already exists")
	// ErrTooManyFiles: the user has MaxFiles files.
	ErrTooManyFiles = errors.New("too many files")
	// ErrQuotaExceeded: the files would take more than MaxTotalBytes.
	ErrQuotaExceeded = errors.New("not enough room left for files")
)

// Limits bound what one user can keep. A zero field takes its default.
type Limits struct {
	MaxFileBytes  int
	MaxFiles      int
	MaxTotalBytes int64
	MaxReadBytes  int
}

// DefaultLimits are 64 KB a file, 200 files, 20 MB together, 4 000 bytes a reading.
func DefaultLimits() Limits {
	return Limits{
		MaxFileBytes:  DefaultMaxFileBytes,
		MaxFiles:      DefaultMaxFiles,
		MaxTotalBytes: DefaultMaxTotalBytes,
		MaxReadBytes:  DefaultMaxReadBytes,
	}
}

func (l Limits) withDefaults() Limits {
	d := DefaultLimits()
	if l.MaxFileBytes <= 0 {
		l.MaxFileBytes = d.MaxFileBytes
	}
	if l.MaxFiles <= 0 {
		l.MaxFiles = d.MaxFiles
	}
	if l.MaxTotalBytes <= 0 {
		l.MaxTotalBytes = d.MaxTotalBytes
	}
	if l.MaxReadBytes <= 0 {
		l.MaxReadBytes = d.MaxReadBytes
	}
	return l
}

// Info describes a file.
type Info struct {
	Name     string    `json:"name"`
	Size     int64     `json:"size"`
	Modified time.Time `json:"modified"`
}

// Part is a piece of a file, whole lines.
type Part struct {
	Name       string
	TotalLines int
	// StartLine and EndLine are the first and the last line returned (1-based); a Part
	// past the end has EndLine = TotalLines and no Content.
	StartLine int
	EndLine   int
	// NextStartLine is where to read on, 0 when the file ends here.
	NextStartLine int
	Content       string
	// Truncated: one line was longer than the reading allows and was cut.
	Truncated bool
}

// Store is the folder of all users.
type Store struct {
	base   *os.Root
	limits Limits

	mu    sync.Mutex
	locks map[uuid.UUID]*sync.Mutex
}

// Open makes the folder if it is not there and opens it. dir must be an absolute path,
// and not a long one.
func Open(dir string, limits Limits) (*Store, error) {
	if !filepath.IsAbs(dir) {
		return nil, fmt.Errorf("userfiles: the folder %q is not an absolute path", dir)
	}
	if len(dir) > MaxFolderLen {
		return nil, fmt.Errorf("userfiles: the path of the folder is %d characters long; the limit is %d", len(dir), MaxFolderLen)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("userfiles: failed to create the folder: %w", err)
	}
	base, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("userfiles: failed to open the folder: %w", err)
	}
	return &Store{base: base, limits: limits.withDefaults(), locks: map[uuid.UUID]*sync.Mutex{}}, nil
}

// Close releases the folder.
func (s *Store) Close() error { return s.base.Close() }

// Limits returns the limits in force.
func (s *Store) Limits() Limits { return s.limits }

// lock serialises the writes of one user, so that the limits are checked and used in one
// step. The returned function unlocks.
func (s *Store) lock(userID uuid.UUID) func() {
	s.mu.Lock()
	m, ok := s.locks[userID]
	if !ok {
		m = &sync.Mutex{}
		s.locks[userID] = m
	}
	s.mu.Unlock()
	m.Lock()
	return m.Unlock
}

// userRoot opens the user's folder, making it on first use. The user comes from the
// session; the model never names one.
func (s *Store) userRoot(userID uuid.UUID) (*os.Root, error) {
	if userID == uuid.Nil {
		return nil, errors.New("userfiles: no user")
	}
	name := userID.String()
	if err := s.base.Mkdir(name, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return nil, fmt.Errorf("userfiles: failed to make the folder of the user: %w", err)
	}
	root, err := s.base.OpenRoot(name)
	if err != nil {
		return nil, fmt.Errorf("userfiles: failed to open the folder of the user: %w", err)
	}
	return root, nil
}

// CheckContent says whether text may be written: not empty, valid UTF-8, no NUL or other
// control characters except tab and line breaks, and not over the limit of one file.
func (s *Store) CheckContent(content string) error {
	if strings.TrimSpace(content) == "" {
		return invalid("the text is empty")
	}
	if len(content) > s.limits.MaxFileBytes {
		return invalid("the text is %d bytes; a file may hold %d. Write a shorter text, or add the rest to the file in parts", len(content), s.limits.MaxFileBytes)
	}
	if !utf8.ValidString(content) {
		return invalid("the text is not valid UTF-8")
	}
	for _, r := range content {
		if r < 0x20 && r != '\t' && r != '\n' && r != '\r' {
			return invalid("the text has a control character; send plain text")
		}
		if r == 0x7f {
			return invalid("the text has a control character; send plain text")
		}
	}
	return nil
}

// List returns the files the model may use, by name. Anything else in the folder (a
// subfolder, a link, a file of another kind, a temporary file) is not listed.
func (s *Store) List(userID uuid.UUID) ([]Info, error) {
	root, err := s.userRoot(userID)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	return listFiles(root)
}

func listFiles(root *os.Root) ([]Info, error) {
	entries, err := fs.ReadDir(root.FS(), ".")
	if err != nil {
		return nil, fmt.Errorf("userfiles: failed to list the files: %w", err)
	}
	files := make([]Info, 0, len(entries))
	for _, e := range entries {
		if ValidateName(e.Name()) != nil {
			continue
		}
		info, err := e.Info()
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		files = append(files, Info{Name: e.Name(), Size: info.Size(), Modified: info.ModTime()})
	}
	return files, nil
}

type usage struct {
	files int
	bytes int64
}

// usageOf counts what the user has: the number of files and, in bytes, the files and
// the previous versions together.
func usageOf(root *os.Root) (usage, error) {
	var u usage
	entries, err := fs.ReadDir(root.FS(), ".")
	if err != nil {
		return u, fmt.Errorf("userfiles: failed to count the files: %w", err)
	}
	for _, e := range entries {
		if e.IsDir() {
			if e.Name() == previousDir {
				if old, err := fs.ReadDir(root.FS(), previousDir); err == nil {
					for _, o := range old {
						if info, err := o.Info(); err == nil && info.Mode().IsRegular() {
							u.bytes += info.Size()
						}
					}
				}
			}
			continue
		}
		info, err := e.Info()
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		u.bytes += info.Size()
		if ValidateName(e.Name()) == nil {
			u.files++
		}
	}
	return u, nil
}

// sweep removes the temporary files a write that died halfway left behind.
func sweep(root *os.Root) {
	entries, err := fs.ReadDir(root.FS(), ".")
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), tempPrefix) {
			continue
		}
		if info, err := e.Info(); err == nil && time.Since(info.ModTime()) > staleTempAge {
			_ = root.Remove(e.Name())
		}
	}
}

// Read returns up to maxLines whole lines of a file from startLine (1-based; 0 means
// the first), and not more than MaxReadBytes of them.
func (s *Store) Read(userID uuid.UUID, name string, startLine, maxLines int) (Part, error) {
	if err := ValidateName(name); err != nil {
		return Part{}, err
	}
	root, err := s.userRoot(userID)
	if err != nil {
		return Part{}, err
	}
	defer root.Close()

	data, err := readBounded(root, name, s.limits.MaxFileBytes*readCapFactor)
	if err != nil {
		return Part{}, err
	}
	if !utf8.Valid(data) {
		return Part{}, invalid("%s is not a text file in UTF-8", name)
	}
	text := strings.TrimPrefix(string(data), "\xef\xbb\xbf") // a byte order mark
	lines := splitLines(text)

	if startLine < 1 {
		startLine = 1
	}
	if maxLines < 1 {
		maxLines = DefaultReadLines
	}
	part := Part{Name: name, TotalLines: len(lines), StartLine: startLine}
	if startLine > len(lines) {
		part.EndLine = len(lines)
		return part, nil
	}

	var out strings.Builder
	used := 0
	i := startLine - 1
	for ; i < len(lines) && i-(startLine-1) < maxLines; i++ {
		line := lines[i]
		need := len(line)
		if used > 0 {
			need++
		}
		if used+need > s.limits.MaxReadBytes {
			if used == 0 {
				line = cutAtRune(line, s.limits.MaxReadBytes)
				part.Truncated = true
				out.WriteString(line)
				i++
			}
			break
		}
		if used > 0 {
			out.WriteByte('\n')
		}
		out.WriteString(line)
		used += need
	}
	part.Content = out.String()
	part.EndLine = i
	if i < len(lines) {
		part.NextStartLine = i + 1
	}
	return part, nil
}

// splitLines splits text into lines without their line breaks; a final line break does
// not make an empty last line.
func splitLines(text string) []string {
	if text == "" {
		return nil
	}
	lines := strings.Split(text, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	for i, l := range lines {
		lines[i] = strings.TrimSuffix(l, "\r")
	}
	return lines
}

func cutAtRune(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	cut := maxBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// readBounded reads a regular file of at most max bytes.
func readBounded(root *os.Root, name string, max int) ([]byte, error) {
	f, err := root.Open(name)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("userfiles: failed to open %s: %w", name, err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("userfiles: failed to read %s: %w", name, err)
	}
	if !info.Mode().IsRegular() {
		return nil, ErrNotFound
	}
	if info.Size() > int64(max) {
		return nil, invalid("%s is too big to read here (%d bytes)", name, info.Size())
	}
	data, err := io.ReadAll(io.LimitReader(f, int64(max)+1))
	if err != nil {
		return nil, fmt.Errorf("userfiles: failed to read %s: %w", name, err)
	}
	return data, nil
}

// Create writes a new file. It never replaces one: if the name is taken it fails with
// ErrExists, and the caller decides what to do (Replace, after the user agreed).
func (s *Store) Create(userID uuid.UUID, name, content string) (Info, error) {
	if err := ValidateName(name); err != nil {
		return Info{}, err
	}
	if err := s.CheckContent(content); err != nil {
		return Info{}, err
	}
	unlock := s.lock(userID)
	defer unlock()
	root, err := s.userRoot(userID)
	if err != nil {
		return Info{}, err
	}
	defer root.Close()
	sweep(root)

	u, err := usageOf(root)
	if err != nil {
		return Info{}, err
	}
	if u.files >= s.limits.MaxFiles {
		return Info{}, ErrTooManyFiles
	}
	if u.bytes+int64(len(content)) > s.limits.MaxTotalBytes {
		return Info{}, ErrQuotaExceeded
	}

	f, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			return Info{}, ErrExists
		}
		return Info{}, fmt.Errorf("userfiles: failed to create %s: %w", name, err)
	}
	if err := writeAndClose(f, content); err != nil {
		_ = root.Remove(name)
		return Info{}, fmt.Errorf("userfiles: failed to write %s: %w", name, err)
	}
	return statInfo(root, name)
}

// Replace puts new text in place of the whole file. The version it replaces is kept in
// the folder of previous versions (one generation); if that cannot be done, nothing is
// changed. The new text is written to a temporary file first and renamed over the old
// one, so a reader sees all of the old file or all of the new one.
func (s *Store) Replace(userID uuid.UUID, name, content string) (Info, error) {
	if err := ValidateName(name); err != nil {
		return Info{}, err
	}
	if err := s.CheckContent(content); err != nil {
		return Info{}, err
	}
	unlock := s.lock(userID)
	defer unlock()
	root, err := s.userRoot(userID)
	if err != nil {
		return Info{}, err
	}
	defer root.Close()
	sweep(root)

	old, err := readBounded(root, name, s.limits.MaxFileBytes*readCapFactor)
	if err != nil {
		return Info{}, err
	}
	u, err := usageOf(root)
	if err != nil {
		return Info{}, err
	}
	// After the swap the folder holds the new text in place of the old file, and the old
	// file as the previous version in place of the earlier previous version of the same
	// name: at most u.bytes + len(content).
	if u.bytes+int64(len(content)) > s.limits.MaxTotalBytes {
		return Info{}, ErrQuotaExceeded
	}

	tmp, err := writeTemp(root, content)
	if err != nil {
		return Info{}, err
	}
	if err := keepPrevious(root, name, old); err != nil {
		_ = root.Remove(tmp)
		return Info{}, err
	}
	if err := root.Rename(tmp, name); err != nil {
		_ = root.Remove(tmp)
		return Info{}, fmt.Errorf("userfiles: failed to replace %s: %w", name, err)
	}
	return statInfo(root, name)
}

// Append adds text to the end of a file that exists. If the file does not end with a line
// break, one is put in front of the text, so that a list keeps one entry to a line.
func (s *Store) Append(userID uuid.UUID, name, text string) (Info, error) {
	if err := ValidateName(name); err != nil {
		return Info{}, err
	}
	if strings.TrimSpace(text) == "" {
		return Info{}, invalid("the text to add is empty")
	}
	if err := s.CheckContent(text); err != nil {
		return Info{}, err
	}
	unlock := s.lock(userID)
	defer unlock()
	root, err := s.userRoot(userID)
	if err != nil {
		return Info{}, err
	}
	defer root.Close()
	sweep(root)

	f, err := root.OpenFile(name, os.O_RDWR|os.O_APPEND, 0)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Info{}, ErrNotFound
		}
		return Info{}, fmt.Errorf("userfiles: failed to open %s: %w", name, err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return Info{}, fmt.Errorf("userfiles: failed to read %s: %w", name, err)
	}
	if !info.Mode().IsRegular() {
		return Info{}, ErrNotFound
	}

	prefix := ""
	if info.Size() > 0 {
		last := make([]byte, 1)
		if _, err := f.ReadAt(last, info.Size()-1); err == nil && last[0] != '\n' {
			prefix = "\n"
		}
	}
	addition := prefix + text
	if info.Size()+int64(len(addition)) > int64(s.limits.MaxFileBytes) {
		return Info{}, invalid("the file would be %d bytes; a file may hold %d. Start another file for the rest", info.Size()+int64(len(addition)), s.limits.MaxFileBytes)
	}
	u, err := usageOf(root)
	if err != nil {
		return Info{}, err
	}
	if u.bytes+int64(len(addition)) > s.limits.MaxTotalBytes {
		return Info{}, ErrQuotaExceeded
	}
	if _, err := f.WriteString(addition); err != nil {
		return Info{}, fmt.Errorf("userfiles: failed to write %s: %w", name, err)
	}
	if err := f.Sync(); err != nil {
		return Info{}, fmt.Errorf("userfiles: failed to write %s: %w", name, err)
	}
	return statInfo(root, name)
}

// Previous returns the version a Replace took the place of, if there is one.
func (s *Store) Previous(userID uuid.UUID, name string) (string, error) {
	if err := ValidateName(name); err != nil {
		return "", err
	}
	root, err := s.userRoot(userID)
	if err != nil {
		return "", err
	}
	defer root.Close()
	prev, err := root.OpenRoot(previousDir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", ErrNotFound
		}
		return "", fmt.Errorf("userfiles: failed to open the previous versions: %w", err)
	}
	defer prev.Close()
	data, err := readBounded(prev, name, s.limits.MaxFileBytes*readCapFactor)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// keepPrevious saves old as the previous version of name.
func keepPrevious(root *os.Root, name string, old []byte) error {
	if err := root.Mkdir(previousDir, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return fmt.Errorf("userfiles: failed to make the folder of previous versions: %w", err)
	}
	prev, err := root.OpenRoot(previousDir)
	if err != nil {
		return fmt.Errorf("userfiles: failed to open the folder of previous versions: %w", err)
	}
	defer prev.Close()
	if err := prev.WriteFile(name, old, 0o600); err != nil {
		return fmt.Errorf("userfiles: failed to keep the previous version of %s: %w", name, err)
	}
	return nil
}

// writeTemp writes content to a new temporary file in the folder and returns its name.
func writeTemp(root *os.Root, content string) (string, error) {
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("userfiles: no random name for a temporary file: %w", err)
	}
	tmp := tempPrefix + hex.EncodeToString(raw[:])
	f, err := root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", fmt.Errorf("userfiles: failed to make a temporary file: %w", err)
	}
	if err := writeAndClose(f, content); err != nil {
		_ = root.Remove(tmp)
		return "", fmt.Errorf("userfiles: failed to write a temporary file: %w", err)
	}
	return tmp, nil
}

// writeAndClose writes content, makes it durable and closes the file.
func writeAndClose(f *os.File, content string) error {
	if _, err := f.WriteString(content); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

func statInfo(root *os.Root, name string) (Info, error) {
	info, err := root.Stat(name)
	if err != nil {
		return Info{}, fmt.Errorf("userfiles: failed to read %s: %w", name, err)
	}
	return Info{Name: name, Size: info.Size(), Modified: info.ModTime()}, nil
}

// Stat describes a file that exists. ErrNotFound if there is none (a folder or a link is
// not a file).
func (s *Store) Stat(userID uuid.UUID, name string) (Info, error) {
	if err := ValidateName(name); err != nil {
		return Info{}, err
	}
	root, err := s.userRoot(userID)
	if err != nil {
		return Info{}, err
	}
	defer root.Close()
	info, err := root.Lstat(name)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Info{}, ErrNotFound
		}
		return Info{}, fmt.Errorf("userfiles: failed to read %s: %w", name, err)
	}
	if !info.Mode().IsRegular() {
		return Info{}, ErrNotFound
	}
	return Info{Name: name, Size: info.Size(), Modified: info.ModTime()}, nil
}

// Usage counts the user's files and the bytes they take together with the previous
// versions: what the limits are measured against.
func (s *Store) Usage(userID uuid.UUID) (files int, bytes int64, err error) {
	root, err := s.userRoot(userID)
	if err != nil {
		return 0, 0, err
	}
	defer root.Close()
	u, err := usageOf(root)
	if err != nil {
		return 0, 0, err
	}
	return u.files, u.bytes, nil
}

// Text returns the whole text of a file, for the user to read: the files the model
// writes are small, so there is no need to read them in parts as the model does.
func (s *Store) Text(userID uuid.UUID, name string) (string, Info, error) {
	if err := ValidateName(name); err != nil {
		return "", Info{}, err
	}
	root, err := s.userRoot(userID)
	if err != nil {
		return "", Info{}, err
	}
	defer root.Close()
	data, err := readBounded(root, name, s.limits.MaxFileBytes*readCapFactor)
	if err != nil {
		return "", Info{}, err
	}
	if !utf8.Valid(data) {
		return "", Info{}, invalid("%s is not a text file in UTF-8", name)
	}
	info, err := statInfo(root, name)
	if err != nil {
		return "", Info{}, err
	}
	return strings.TrimPrefix(string(data), "\xef\xbb\xbf"), info, nil
}

// Delete removes a file and its previous version. It is for the user, through the HTTP
// routes; the model has no tool that calls it.
func (s *Store) Delete(userID uuid.UUID, name string) error {
	if err := ValidateName(name); err != nil {
		return err
	}
	unlock := s.lock(userID)
	defer unlock()
	root, err := s.userRoot(userID)
	if err != nil {
		return err
	}
	defer root.Close()

	info, err := root.Lstat(name)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return ErrNotFound
		}
		return fmt.Errorf("userfiles: failed to read %s: %w", name, err)
	}
	if !info.Mode().IsRegular() {
		return ErrNotFound
	}
	if err := root.Remove(name); err != nil {
		return fmt.Errorf("userfiles: failed to delete %s: %w", name, err)
	}
	if prev, err := root.OpenRoot(previousDir); err == nil {
		_ = prev.Remove(name)
		_ = prev.Close()
	}
	return nil
}
