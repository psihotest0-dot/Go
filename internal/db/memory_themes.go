package db

import (
	"encoding/json"
	"strings"

	"github.com/sqlc-dev/pqtype"
)

// memoryThemesKey is the key of a memory's metadata under which the background job of step 2.12
// (jobs.MemoryThemesWorker) keeps the theme words of the memory: when it would help to recall it
// ("умора", "почивка", "вечер"). The server writes it; a client cannot.
const memoryThemesKey = "themes"

// MemoryThemes returns the theme words of a memory's metadata, nil when it has none. An empty,
// non-nil slice means the job looked at the memory and the model gave it no themes.
func MemoryThemes(metadata pqtype.NullRawMessage) []string {
	if !metadata.Valid {
		return nil
	}
	var meta struct {
		Themes *[]string `json:"themes"`
	}
	if json.Unmarshal(metadata.RawMessage, &meta) != nil || meta.Themes == nil {
		return nil
	}
	return *meta.Themes
}

// HasMemoryThemes tells whether the job has already looked at the memory (themes may be empty).
func HasMemoryThemes(metadata pqtype.NullRawMessage) bool {
	return metadata.Valid && objectKey(metadata.RawMessage, memoryThemesKey) != nil
}

// MemoryEmbeddingText is the text the vector of a memory is made from: "<themes>: <content>" when
// the memory has themes, else the content alone. Measured on 2026-11-22 (measurements\
// memthemes_2026-11-22): with the themes the right memory came first for 29 of 37 personal
// messages instead of 24. Every place that makes a memory's vector from stored rows goes through
// it (the job, cmd/reembed); a new or edited memory has no themes yet, so its content is enough.
func MemoryEmbeddingText(content string, metadata pqtype.NullRawMessage) string {
	var themes []string
	for _, t := range MemoryThemes(metadata) {
		if t = strings.Join(strings.Fields(t), " "); t != "" {
			themes = append(themes, t)
		}
	}
	if len(themes) == 0 {
		return content
	}
	return strings.Join(themes, ", ") + ": " + content
}

// WithoutMemoryThemes drops the themes from metadata a client sent: only the job writes them, and
// themes that do not match the vector would never be made again.
func WithoutMemoryThemes(metadata pqtype.NullRawMessage) pqtype.NullRawMessage {
	if !metadata.Valid {
		return metadata
	}
	object := map[string]json.RawMessage{}
	if json.Unmarshal(metadata.RawMessage, &object) != nil {
		return metadata
	}
	if _, ok := object[memoryThemesKey]; !ok {
		return metadata
	}
	delete(object, memoryThemesKey)
	raw, err := json.Marshal(object)
	if err != nil {
		return metadata
	}
	return pqtype.NullRawMessage{RawMessage: raw, Valid: true}
}

// MemoryMetadataForUpdate is the metadata to write when a client changes a memory (PUT). requested
// is what the client sent (not Valid: keep the stored metadata), stored is the row's metadata. The
// themes belong to the text: a new text loses them, and the vector is made from the text alone
// until the job gives it new ones; the same text keeps the stored themes, whatever the client sent.
func MemoryMetadataForUpdate(requested, stored pqtype.NullRawMessage, contentChanged bool) pqtype.NullRawMessage {
	if !requested.Valid {
		if !contentChanged || !HasMemoryThemes(stored) {
			return requested // the query keeps the stored metadata
		}
		return WithoutMemoryThemes(stored)
	}
	out := WithoutMemoryThemes(requested)
	if contentChanged || !HasMemoryThemes(stored) {
		return out
	}
	object := map[string]json.RawMessage{}
	if json.Unmarshal(out.RawMessage, &object) != nil {
		return out
	}
	object[memoryThemesKey] = objectKey(stored.RawMessage, memoryThemesKey)
	raw, err := json.Marshal(object)
	if err != nil {
		return out
	}
	return pqtype.NullRawMessage{RawMessage: raw, Valid: true}
}

// objectKey is the raw value of key in a JSON object, nil when it is not there.
func objectKey(raw []byte, key string) json.RawMessage {
	object := map[string]json.RawMessage{}
	if json.Unmarshal(raw, &object) != nil {
		return nil
	}
	return object[key]
}
