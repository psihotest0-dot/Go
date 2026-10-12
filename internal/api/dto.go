package api

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"go-monolith/internal/db"
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

type MessageDTO struct {
	ID        uuid.UUID  `json:"id"`
	UserID    uuid.UUID  `json:"user_id"`
	SessionID string     `json:"session_id"`
	Role      string     `json:"role"`
	Content   string     `json:"content"`
	CreatedAt *time.Time `json:"created_at"`
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
	ID        uuid.UUID  `json:"id"`
	UserID    uuid.UUID  `json:"user_id"`
	FileName  string     `json:"file_name"`
	MimeType  string     `json:"mime_type"`
	FileSize  int64      `json:"file_size"`
	Status    string     `json:"status"`
	CreatedAt *time.Time `json:"created_at"`
	UpdatedAt *time.Time `json:"updated_at"`
}

func toIngestedFileDTO(f db.IngestedFile) IngestedFileDTO {
	dto := IngestedFileDTO{
		ID:       f.ID,
		UserID:   f.UserID,
		FileName: f.FileName,
		MimeType: f.MimeType,
		FileSize: f.FileSize,
		Status:   f.Status,
	}
	if f.CreatedAt.Valid {
		dto.CreatedAt = &f.CreatedAt.Time
	}
	if f.UpdatedAt.Valid {
		dto.UpdatedAt = &f.UpdatedAt.Time
	}
	return dto
}
