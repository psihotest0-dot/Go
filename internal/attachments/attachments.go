// Package attachments holds the text files a user attaches to one conversation: code,
// a log, a long paste turned into a file.
//
// They are not the library. The library splits a book into passages, embeds and
// summarises them and the model finds pieces by meaning; that is the wrong tool for
// a source file, where the model needs the exact lines in order. An attachment is
// stored as it is, belongs to one session, and reaches the model in two ways: the
// files that fit a small budget are put into the prompt with numbered lines, and any
// file can be read in pieces through the read_attachment tool.
package attachments

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"go-monolith/internal/db"
	"go-monolith/metrics"
)

const (
	// MaxFileBytes is the largest file: about 6 000 lines of code. A bigger one is not a
	// thing to hand to a model with a window of 16 000 tokens.
	MaxFileBytes = 256 << 10
	// MaxSessionFiles and MaxSessionBytes bound one conversation.
	MaxSessionFiles = 20
	MaxSessionBytes = 1 << 20
	// MaxNameRunes bounds a file name.
	MaxNameRunes = 120
	// MaxSessionIDRunes matches the check in the database.
	MaxSessionIDRunes = 128

	// StaleAfter is how long an attachment lives without being attached again.
	// Conversations are not deleted anywhere, so age is the only clean-up.
	StaleAfter = 30 * 24 * time.Hour

	// BytesPerToken is the cost of a token for code. It is measured (Gemma, Python and Go:
	// 3.1 bytes per token) and is on the safe side of the 4 the rest of the agent uses for
	// prose, because attached code is read in full and a window overflow fails the turn.
	BytesPerToken = 3

	// MaxLineChars cuts a single very long line (minified code, a log line) so that one
	// line cannot take the whole window.
	MaxLineChars = 2000
)

// The reasons an attachment is refused; each is a mistake of the caller.
var (
	ErrBadSession  = errors.New("session_id must be 1 to 128 characters without control characters")
	ErrBadName     = errors.New("name must be a file name of 1 to 120 characters")
	ErrEmpty       = errors.New("the file is empty")
	ErrBinary      = errors.New("the file is not text (it contains a NUL byte)")
	ErrTooLarge    = errors.New("the file is too large (the limit is 256 KB)")
	ErrTooMany     = errors.New("this conversation already has 20 attached files")
	ErrSessionFull = errors.New("the attached files of this conversation would be over 1 MB")
	ErrNotFound    = errors.New("attachment not found")
	ErrAmbiguous   = errors.New("two attached files have that name in other letter case")
)

// Info describes an attached file; it is what lists and uploads answer with.
type Info struct {
	ID        uuid.UUID `json:"id"`
	Name      string    `json:"name"`
	Bytes     int       `json:"bytes"`
	Lines     int       `json:"lines"`
	Tokens    int       `json:"tokens"`
	Version   int       `json:"version"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// File is an attached file with its content.
type File struct {
	Info
	Content string `json:"-"`
}

// Added is the answer to an upload.
type Added struct {
	Info
	// Created is true for a name that was not attached before.
	Created bool `json:"created"`
	// Replaced is true when the name was attached before with other content.
	Replaced bool `json:"replaced"`
	// Unchanged is true when the same content was attached again.
	Unchanged bool `json:"unchanged"`
}

// Service stores and reads attachments.
type Service struct {
	db  *db.Database
	now func() time.Time
}

// NewService creates the service.
func NewService(database *db.Database) *Service {
	return &Service{db: database, now: time.Now}
}

// EstimateTokens is the cost of n bytes of attached text in tokens.
func EstimateTokens(n int) int {
	if n <= 0 {
		return 0
	}
	return (n + BytesPerToken - 1) / BytesPerToken
}

// CleanName turns what a client sends into a file name: only the last part of a path,
// no control characters, no surrounding spaces.
func CleanName(name string) (string, error) {
	if i := strings.LastIndexAny(name, `/\`); i >= 0 {
		name = name[i+1:]
	}
	name = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, name)
	name = strings.TrimSpace(name)
	if name == "" || name == "." || name == ".." || utf8.RuneCountInString(name) > MaxNameRunes {
		return "", ErrBadName
	}
	return name, nil
}

// NormalizeContent makes the text what everything else assumes: no byte order mark, LF
// line breaks. It refuses empty and binary content and anything over MaxFileBytes.
func NormalizeContent(content string) (string, error) {
	content = strings.TrimPrefix(content, "\xef\xbb\xbf")
	if strings.IndexByte(content, 0) >= 0 {
		return "", ErrBinary
	}
	content = strings.ReplaceAll(content, "\r\n", "\n")
	content = strings.ReplaceAll(content, "\r", "\n")
	if strings.TrimSpace(content) == "" {
		return "", ErrEmpty
	}
	if len(content) > MaxFileBytes {
		return "", ErrTooLarge
	}
	return content, nil
}

// SplitLines splits text into lines; a final line break does not make an empty last line.
func SplitLines(content string) []string {
	if content == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(content, "\n"), "\n")
}

func validSession(sessionID string) bool {
	n := utf8.RuneCountInString(sessionID)
	if n < 1 || n > MaxSessionIDRunes {
		return false
	}
	for _, r := range sessionID {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func hashOf(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

func info(id uuid.UUID, name string, bytes, lines, version int, created, updated time.Time) Info {
	return Info{ID: id, Name: name, Bytes: bytes, Lines: lines, Tokens: EstimateTokens(bytes),
		Version: version, CreatedAt: created, UpdatedAt: updated}
}

// Add attaches a file to a session. The same name again is a new version of the file;
// the same content again changes nothing.
func (s *Service) Add(ctx context.Context, userID uuid.UUID, sessionID, name, content string) (Added, error) {
	added, err := s.add(ctx, userID, sessionID, name, content)
	if err != nil {
		if errors.Is(err, ErrBadSession) || errors.Is(err, ErrBadName) || errors.Is(err, ErrEmpty) || errors.Is(err, ErrBinary) ||
			errors.Is(err, ErrTooLarge) || errors.Is(err, ErrTooMany) || errors.Is(err, ErrSessionFull) {
			metrics.ChatAttachments.WithLabelValues("rejected").Inc()
		}
		return Added{}, err
	}
	switch {
	case added.Unchanged:
		metrics.ChatAttachments.WithLabelValues("unchanged").Inc()
	case added.Replaced:
		metrics.ChatAttachments.WithLabelValues("replaced").Inc()
	default:
		metrics.ChatAttachments.WithLabelValues("added").Inc()
	}
	return added, nil
}

func (s *Service) add(ctx context.Context, userID uuid.UUID, sessionID, name, content string) (Added, error) {
	if !validSession(sessionID) {
		return Added{}, ErrBadSession
	}
	name, err := CleanName(name)
	if err != nil {
		return Added{}, err
	}
	content, err = NormalizeContent(content)
	if err != nil {
		return Added{}, err
	}
	hash := hashOf(content)
	lines := len(SplitLines(content))

	// Old files go first, so a user who attaches a lot is not stopped by files nobody
	// opens any more.
	if n, err := s.db.Queries.DeleteStaleChatAttachments(ctx, db.DeleteStaleChatAttachmentsParams{UserID: userID, UpdatedAt: s.now().Add(-StaleAfter)}); err != nil {
		slog.WarnContext(ctx, "failed to remove stale attachments", "err", err)
	} else if n > 0 {
		slog.InfoContext(ctx, "removed stale attachments", "count", n)
	}

	existing, err := s.db.Queries.GetChatAttachmentMeta(ctx, db.GetChatAttachmentMetaParams{UserID: userID, SessionID: sessionID, Name: name})
	known := err == nil
	if err != nil && !errors.Is(err, pgx.ErrNoRows) && !errors.Is(err, sql.ErrNoRows) {
		return Added{}, fmt.Errorf("failed to look up the attachment: %w", err)
	}

	totals, err := s.db.Queries.SumChatAttachmentBytes(ctx, db.SumChatAttachmentBytesParams{UserID: userID, SessionID: sessionID})
	if err != nil {
		return Added{}, fmt.Errorf("failed to count the attachments: %w", err)
	}
	if !known && totals.Files >= MaxSessionFiles {
		return Added{}, ErrTooMany
	}
	total := totals.TotalBytes + int64(len(content))
	if known {
		total -= int64(existing.Bytes)
	}
	if total > MaxSessionBytes {
		return Added{}, ErrSessionFull
	}

	row, err := s.db.Queries.UpsertChatAttachment(ctx, db.UpsertChatAttachmentParams{
		UserID: userID, SessionID: sessionID, Name: name, Content: content,
		ContentHash: hash, Bytes: int32(len(content)), LineCount: int32(lines),
	})
	if err != nil {
		return Added{}, fmt.Errorf("failed to store the attachment: %w", err)
	}

	result := Added{
		Info:      info(row.ID, row.Name, int(row.Bytes), int(row.LineCount), int(row.Version), row.CreatedAt, row.UpdatedAt),
		Created:   !known,
		Replaced:  known && existing.ContentHash != hash,
		Unchanged: known && existing.ContentHash == hash,
	}
	slog.InfoContext(ctx, "chat attachment stored", "attachment_id", row.ID, "session_id", sessionID,
		"bytes", row.Bytes, "lines", row.LineCount, "version", row.Version, "created", result.Created, "replaced", result.Replaced)
	return result, nil
}

// List describes the files attached to a session, newest first, without reading them.
func (s *Service) List(ctx context.Context, userID uuid.UUID, sessionID string) ([]Info, error) {
	if !validSession(sessionID) {
		return nil, ErrBadSession
	}
	rows, err := s.db.Queries.ListChatAttachments(ctx, db.ListChatAttachmentsParams{UserID: userID, SessionID: sessionID})
	if err != nil {
		return nil, fmt.Errorf("failed to list the attachments: %w", err)
	}
	out := make([]Info, 0, len(rows))
	for _, r := range rows {
		out = append(out, info(r.ID, r.Name, int(r.Bytes), int(r.LineCount), int(r.Version), r.CreatedAt, r.UpdatedAt))
	}
	return out, nil
}

// Files returns the files attached to a session with their content, newest first.
func (s *Service) Files(ctx context.Context, userID uuid.UUID, sessionID string) ([]File, error) {
	if !validSession(sessionID) {
		return nil, ErrBadSession
	}
	rows, err := s.db.Queries.ListChatAttachmentsWithContent(ctx, db.ListChatAttachmentsWithContentParams{UserID: userID, SessionID: sessionID})
	if err != nil {
		return nil, fmt.Errorf("failed to read the attachments: %w", err)
	}
	out := make([]File, 0, len(rows))
	for _, r := range rows {
		out = append(out, File{Info: info(r.ID, r.Name, int(r.Bytes), int(r.LineCount), int(r.Version), r.CreatedAt, r.UpdatedAt), Content: r.Content})
	}
	return out, nil
}

// Find returns the file with that name in the session. The name must match exactly or,
// failing that, in one single other letter case.
func (s *Service) Find(ctx context.Context, userID uuid.UUID, sessionID, name string) (File, error) {
	if !validSession(sessionID) {
		return File{}, ErrBadSession
	}
	name = strings.TrimSpace(name)
	row, err := s.db.Queries.GetChatAttachmentByName(ctx, db.GetChatAttachmentByNameParams{UserID: userID, SessionID: sessionID, Name: name})
	if err == nil {
		return File{Info: info(row.ID, row.Name, int(row.Bytes), int(row.LineCount), int(row.Version), row.CreatedAt, row.UpdatedAt), Content: row.Content}, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) && !errors.Is(err, sql.ErrNoRows) {
		return File{}, fmt.Errorf("failed to read the attachment: %w", err)
	}
	rows, err := s.db.Queries.FindChatAttachmentsByLowerName(ctx, db.FindChatAttachmentsByLowerNameParams{UserID: userID, SessionID: sessionID, Lower: name})
	if err != nil {
		return File{}, fmt.Errorf("failed to read the attachment: %w", err)
	}
	switch len(rows) {
	case 0:
		return File{}, ErrNotFound
	case 1:
		r := rows[0]
		return File{Info: info(r.ID, r.Name, int(r.Bytes), int(r.LineCount), int(r.Version), r.CreatedAt, r.UpdatedAt), Content: r.Content}, nil
	default:
		return File{}, ErrAmbiguous
	}
}

// Delete removes an attachment of the user.
func (s *Service) Delete(ctx context.Context, userID, id uuid.UUID) error {
	n, err := s.db.Queries.DeleteChatAttachment(ctx, db.DeleteChatAttachmentParams{ID: id, UserID: userID})
	if err != nil {
		return fmt.Errorf("failed to delete the attachment: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	slog.InfoContext(ctx, "chat attachment deleted", "attachment_id", id)
	return nil
}
