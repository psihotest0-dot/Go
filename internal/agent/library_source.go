package agent

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"strings"

	"github.com/google/uuid"

	"go-monolith/internal/db"
	"go-monolith/internal/webfetch"
)

// The link a document of the library was taken from is kept with it (migration 45, 2026-11-28). The
// live check of that day added a page of documentation twice - by add_to_library, then by the task
// over the links - as two files of the same size and another SHA-256: a page changes between two
// downloads, so the content hash did not see it. Now add_to_library, the link check and the task
// over the links look the link up first, and a link that is in the library is not fetched again.

// librarySource is the document of the library taken from the link, if there is one.
func (r *DefaultToolRegistry) librarySource(ctx context.Context, userID uuid.UUID, link string) (db.IngestedFile, bool) {
	key := urlKey(link)
	if r.db == nil || key == "" {
		return db.IngestedFile{}, false
	}
	file, err := r.db.Queries.GetIngestedFileBySourceKey(ctx, db.GetIngestedFileBySourceKeyParams{
		UserID: userID, SourceKey: sql.NullString{String: key, Valid: true},
	})
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			slog.WarnContext(ctx, "could not look up the link in the library; it is fetched", "error", err)
		}
		return db.IngestedFile{}, false
	}
	return file, true
}

// rememberSource keeps the link a document was taken from (the first link of a document stays).
func (r *DefaultToolRegistry) rememberSource(ctx context.Context, userID, fileID uuid.UUID, link string) {
	key := urlKey(link)
	if r.db == nil || key == "" || fileID == uuid.Nil {
		return
	}
	if err := r.db.Queries.SetIngestedFileSourceKey(ctx, db.SetIngestedFileSourceKeyParams{
		ID: fileID, UserID: userID, SourceKey: sql.NullString{String: key, Valid: true},
	}); err != nil {
		// Not kept: the next add of the link fetches it again, as before.
		slog.WarnContext(ctx, "could not keep the link of a document", "error", err)
	}
}

// kindOfFile is the kind of a document of the library by its name, for the link check.
func kindOfFile(name string) webfetch.Kind {
	if strings.HasSuffix(strings.ToLower(name), ".pdf") {
		return webfetch.KindPDF
	}
	return webfetch.KindHTML
}
