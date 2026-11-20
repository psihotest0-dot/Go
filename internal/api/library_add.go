package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
)

// Why a document could not be added to the library, for the caller to tell the user.
var (
	// ErrAddTooLarge: the document is larger than the upload limit.
	ErrAddTooLarge = errors.New("the document is larger than the upload limit")
	// ErrAddEmpty: nothing was received.
	ErrAddEmpty = errors.New("the document is empty")
)

// AddedFile is a document that is now in the library, or was already.
type AddedFile struct {
	ID   uuid.UUID
	Name string
	// Duplicate: the user already had a file with the same content; nothing was added.
	Duplicate bool
	Size      int64
}

// LibraryAdder puts a document that the server fetched itself into the library, by the
// same road as an upload: spooled into the upload folder while it is hashed, refused when
// the user has it already, recorded as pending and queued for ingest. The chat's tool
// add_to_library uses it (plan item P53).
type LibraryAdder struct {
	api   *API
	river *river.Client[pgx.Tx]
}

// NewLibraryAdder makes an adder that queues its ingest jobs on riverClient (nil in tests:
// the row is made and nothing is queued).
func (a *API) NewLibraryAdder(riverClient *river.Client[pgx.Tx]) *LibraryAdder {
	return &LibraryAdder{api: a, river: riverClient}
}

// Add stores src as a file called name in the library of the user. src is read once and not
// closed; a read error (the document passed a limit of its own, the connection broke) stops
// the copy and leaves nothing behind.
func (l *LibraryAdder) Add(ctx context.Context, userID uuid.UUID, name string, src io.Reader) (AddedFile, error) {
	limit := l.api.uploadLimitBytes()
	upload, err := storeUpload(&cappedReader{r: src, limit: limit}, name)
	if err != nil {
		var client *clientError
		if errors.As(err, &client) {
			switch client.status {
			case http.StatusRequestEntityTooLarge:
				return AddedFile{}, ErrAddTooLarge
			case http.StatusBadRequest:
				return AddedFile{}, ErrAddEmpty
			}
			// The reader of the document failed in the middle: say what it said.
			if cause := client.Unwrap(); cause != nil {
				return AddedFile{}, fmt.Errorf("the document could not be stored: %w", cause)
			}
		}
		return AddedFile{}, fmt.Errorf("the document could not be stored: %w", err)
	}

	committed := false
	defer func() {
		if !committed {
			upload.discard()
		}
	}()

	file, existing, err := l.api.registerUpload(ctx, l.river, userID, upload)
	if err != nil {
		return AddedFile{}, err
	}
	if existing {
		return AddedFile{ID: file.ID, Name: file.FileName, Duplicate: true, Size: file.FileSize}, nil
	}
	committed = true
	return AddedFile{ID: file.ID, Name: file.FileName, Size: file.FileSize}, nil
}

// cappedReader reads from r and fails with an *http.MaxBytesError, the error storeUpload
// knows as "file too large", when r gives more than limit bytes.
type cappedReader struct {
	r     io.Reader
	limit int64
	read  int64
}

func (c *cappedReader) Read(p []byte) (int, error) {
	if c.read > c.limit {
		return 0, &http.MaxBytesError{Limit: c.limit}
	}
	if room := c.limit - c.read + 1; int64(len(p)) > room {
		p = p[:room]
	}
	n, err := c.r.Read(p)
	c.read += int64(n)
	if c.read > c.limit {
		return n, &http.MaxBytesError{Limit: c.limit}
	}
	return n, err
}
