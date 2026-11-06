package api

import (
	"encoding/json"
	"math"
	"time"

	"go-monolith/internal/db"

	"github.com/google/uuid"
)

type MemoryDTO struct {
	ID              uuid.UUID        `json:"id"`
	UserID          uuid.UUID        `json:"user_id"`
	Content         string           `json:"content"`
	Status          string           `json:"status"`
	SourceMessageID *uuid.UUID       `json:"source_message_id"`
	Importance      int32            `json:"importance"`
	Metadata        *json.RawMessage `json:"metadata"`
	CreatedAt       *time.Time       `json:"created_at"`
	UpdatedAt       *time.Time       `json:"updated_at"`
	LastUsedAt      *time.Time       `json:"last_used_at"`
}

func toMemoryDTO(m db.Memory) MemoryDTO {
	dto := MemoryDTO{
		ID:         m.ID,
		UserID:     m.UserID,
		Content:    m.Content,
		Status:     m.Status,
		Importance: m.Importance,
	}
	if m.SourceMessageID.Valid {
		dto.SourceMessageID = &m.SourceMessageID.UUID
	}
	if m.Metadata.Valid {
		var rm json.RawMessage = m.Metadata.RawMessage
		dto.Metadata = &rm
	}
	if m.CreatedAt.Valid {
		dto.CreatedAt = &m.CreatedAt.Time
	}
	if m.UpdatedAt.Valid {
		dto.UpdatedAt = &m.UpdatedAt.Time
	}
	if m.LastUsedAt.Valid {
		dto.LastUsedAt = &m.LastUsedAt.Time
	}
	return dto
}

// The ways a source's Score is measured.
const (
	// ScoreTypeDistance: the cosine distance between the question and the source,
	// the unit the search limits are measured in (smaller is closer; the summaries
	// that pass the limit are at 0.10-0.17). It is not a percentage: with the
	// embedding model in use even unrelated texts are at 0.15-0.30, so
	// (1 - distance) * 100 gave 82-85 % for noise.
	ScoreTypeDistance = "distance"
	// ScoreTypeRRF: the reciprocal-rank-fusion score of a passage found by the
	// hybrid search, scaled to 0-100. Only old messages carry it.
	ScoreTypeRRF = "rrf"
)

// RoundDistance keeps three decimals, the precision the limits are given in.
func RoundDistance(d float64) float64 {
	return math.Round(d*1000) / 1000
}

type SourceInfo struct {
	ChunkID        uuid.UUID `json:"chunk_id"`
	FileName       string    `json:"file_name"`
	PageNumber     *int      `json:"page_number,omitempty"`
	ContentSnippet string    `json:"content_snippet"`
	// Score is measured as ScoreType says; a source with no ScoreType (a memory)
	// has no measure and Score is 0.
	Score      float64 `json:"score"`
	ScoreType  string  `json:"score_type,omitempty"`
	SourceType string  `json:"source_type"` // "ocr" | "text" | "concept" | "memory"
}

// historicSourceScore is the Score and ScoreType the history shows for a stored
// source. A record without a type is from before the distance: a summary's score was
// stored as (1 - distance) * 100 - and this code multiplied it by 100 once more, so
// the history showed 8 200 % - and a passage's as a raw RRF score.
func historicSourceScore(c DBChunkInfo, sourceType string) (float64, string) {
	switch {
	case c.ScoreType != "":
		return c.Score, c.ScoreType
	case sourceType == "text" || sourceType == "ocr":
		return (c.Score / 0.033) * 100, ScoreTypeRRF
	default:
		return RoundDistance(1 - c.Score/100), ScoreTypeDistance
	}
}

// DBChunkInfo is a source as it is stored with the answer. A record without a
// ScoreType is from before the distance: its Score is (1 - distance) * 100 for a
// summary and a raw RRF score for a passage.
type DBChunkInfo struct {
	ChunkID   uuid.UUID `json:"chunk_id"`
	Score     float64   `json:"score"`
	ScoreType string    `json:"score_type,omitempty"`
}

type MessageDTO struct {
	ID        uuid.UUID    `json:"id"`
	UserID    uuid.UUID    `json:"user_id"`
	SessionID string       `json:"session_id"`
	Role      string       `json:"role"`
	Content   string       `json:"content"`
	CreatedAt *time.Time   `json:"created_at"`
	Sources   []SourceInfo `json:"sources,omitempty"`
}

func toMessageDTO(m db.Message) MessageDTO {
	dto := MessageDTO{
		ID:        m.ID,
		UserID:    m.UserID,
		SessionID: m.SessionID,
		Role:      m.Role,
		Content:   m.Content,
	}
	if m.CreatedAt.Valid {
		dto.CreatedAt = &m.CreatedAt.Time
	}
	return dto
}

type IngestedFileDTO struct {
	ID             uuid.UUID  `json:"id"`
	UserID         uuid.UUID  `json:"user_id"`
	FileName       string     `json:"file_name"`
	MimeType       string     `json:"mime_type"`
	FileSize       int64      `json:"file_size"`
	Status         string     `json:"status"`
	CreatedAt      *time.Time `json:"created_at"`
	UpdatedAt      *time.Time `json:"updated_at"`
	Category       string     `json:"category"`
	ManualOverride bool       `json:"manual_override"`
	// ChunkCount is the number of embedded chunks stored for the file and
	// HasEmbeddings tells the client whether the file can be used for search.
	ChunkCount      int64  `json:"chunk_count"`
	HasEmbeddings   bool   `json:"has_embeddings"`
	EmbeddingStatus string `json:"embedding_status"`
}

// Embedding statuses reported per file. They extend the ingest status with the
// state the UI cares about most: whether vectors actually exist.
const (
	EmbeddingStatusPending    = "pending"
	EmbeddingStatusProcessing = "processing"
	EmbeddingStatusFailed     = "failed"
	EmbeddingStatusReady      = "completed"
	EmbeddingStatusEmpty      = "empty"
)

// fileEmbeddingStatus derives the embedding status from the ingest status and
// the stored chunk count. A file can finish ingestion with no chunks at all
// (for example a scanned PDF without extractable text), which is reported as
// "empty" instead of "completed" so the client can tell the two apart.
func fileEmbeddingStatus(status string, chunkCount int64) string {
	if status == EmbeddingStatusReady && chunkCount == 0 {
		return EmbeddingStatusEmpty
	}
	return status
}

func toIngestedFileDTO(f db.IngestedFile, chunkCount int64) IngestedFileDTO {
	dto := IngestedFileDTO{
		ID:              f.ID,
		UserID:          f.UserID,
		FileName:        f.FileName,
		MimeType:        f.MimeType,
		FileSize:        f.FileSize,
		Status:          f.Status,
		Category:        f.Category,
		ManualOverride:  f.ManualOverride,
		ChunkCount:      chunkCount,
		HasEmbeddings:   chunkCount > 0,
		EmbeddingStatus: fileEmbeddingStatus(f.Status, chunkCount),
	}
	if f.CreatedAt.Valid {
		dto.CreatedAt = &f.CreatedAt.Time
	}
	if f.UpdatedAt.Valid {
		dto.UpdatedAt = &f.UpdatedAt.Time
	}
	return dto
}
