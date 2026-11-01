package ingest

import (
	"archive/zip"
	"fmt"
	"io"
	"net/url"
	"os"
	"path"
	"strings"
)

// zipLimits bounds what an archive may expand to. A few hundred kilobytes of
// zip can declare hundreds of megabytes of XML (measured: a 193 KB docx holds
// 70 MB of XML, a 199 KB epub 200 MB of text), and the upload limit only counts
// the compressed size.
type zipLimits struct {
	MaxEntries    int   // files in the archive
	MaxTotalBytes int64 // bytes read out of all entries together
}

var defaultZipLimits = zipLimits{
	MaxEntries:    10_000,
	MaxTotalBytes: 200 << 20,
}

// zipArchive is a zip file on disk, opened for reading entries under a shared
// byte budget. The archive is spooled to a temporary file rather than held in
// memory: the compressed bytes alone can be 100 MB, and zip needs random access.
type zipArchive struct {
	reader *zip.Reader
	file   *os.File
	budget *byteBudget
}

// openZipArchive copies r to a temporary file and opens it as a zip archive.
// Close removes the file.
func openZipArchive(r io.Reader, lim zipLimits) (*zipArchive, error) {
	tmp, err := os.CreateTemp("", "ingest-*.zip")
	if err != nil {
		return nil, fmt.Errorf("failed to create a temporary file: %w", err)
	}
	a := &zipArchive{file: tmp, budget: &byteBudget{left: lim.MaxTotalBytes}}

	size, err := io.Copy(tmp, r)
	if err != nil {
		a.Close()
		return nil, err
	}
	a.reader, err = zip.NewReader(tmp, size)
	if err != nil {
		a.Close()
		return nil, fmt.Errorf("%w: not a valid zip archive: %v", ErrUnsupported, err)
	}
	if len(a.reader.File) > lim.MaxEntries {
		a.Close()
		return nil, fmt.Errorf("%w: %d files in the archive, the limit is %d", ErrArchiveTooLarge, len(a.reader.File), lim.MaxEntries)
	}
	return a, nil
}

func (a *zipArchive) Close() {
	name := a.file.Name()
	_ = a.file.Close()
	_ = os.Remove(name)
}

// Path is the location of the spooled archive, for libraries that open files.
func (a *zipArchive) Path() string { return a.file.Name() }

// Find returns the entry with the given name. Names in an archive use forward
// slashes; the match ignores case because generators disagree about it.
func (a *zipArchive) Find(name string) *zip.File {
	for _, f := range a.reader.File {
		if f.Name == name {
			return f
		}
	}
	for _, f := range a.reader.File {
		if strings.EqualFold(f.Name, name) {
			return f
		}
	}
	return nil
}

// Open returns a reader over one entry. It fails at once when the entry
// declares more than the remaining budget, and again while reading if the
// stream turns out to be longer than the header claims.
func (a *zipArchive) Open(f *zip.File) (io.ReadCloser, error) {
	if f.UncompressedSize64 > uint64(a.budget.left) {
		return nil, fmt.Errorf("%w: %q declares %d bytes", ErrArchiveTooLarge, f.Name, f.UncompressedSize64)
	}
	rc, err := f.Open()
	if err != nil {
		return nil, err
	}
	return struct {
		io.Reader
		io.Closer
	}{newCappedReader(rc, a.budget, ErrArchiveTooLarge), rc}, nil
}

// resolveZipPath resolves a link found inside an archive file ("../Text/c1.xhtml#p2",
// possibly percent-encoded) against the directory of that file.
func resolveZipPath(baseFile, ref string) string {
	ref, _, _ = strings.Cut(ref, "#")
	ref, _, _ = strings.Cut(ref, "?")
	if decoded, err := url.PathUnescape(ref); err == nil {
		ref = decoded
	}
	if strings.HasPrefix(ref, "/") {
		return strings.TrimPrefix(path.Clean(ref), "/")
	}
	return path.Clean(path.Join(path.Dir(baseFile), ref))
}
