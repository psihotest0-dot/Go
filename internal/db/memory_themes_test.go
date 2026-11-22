package db

import (
	"encoding/json"
	"testing"

	"github.com/sqlc-dev/pqtype"
	"github.com/stretchr/testify/assert"
)

func meta(s string) pqtype.NullRawMessage {
	return pqtype.NullRawMessage{RawMessage: []byte(s), Valid: true}
}

func object(t *testing.T, m pqtype.NullRawMessage) map[string]any {
	t.Helper()
	if !m.Valid {
		return nil
	}
	var out map[string]any
	assert.NoError(t, json.Unmarshal(m.RawMessage, &out))
	return out
}

func TestMemoryEmbeddingText(t *testing.T) {
	const content = "Почива на дивана пред телевизора."
	assert.Equal(t, content, MemoryEmbeddingText(content, pqtype.NullRawMessage{}))
	assert.Equal(t, content, MemoryEmbeddingText(content, meta(`{"fact_key":"rest"}`)))
	assert.Equal(t, content, MemoryEmbeddingText(content, meta(`{"themes":[]}`)), "looked at, no themes")
	assert.Equal(t, content, MemoryEmbeddingText(content, meta(`{"themes":"умора"}`)), "not a list")
	assert.Equal(t, "умора, вечерна почивка: "+content,
		MemoryEmbeddingText(content, meta(`{"themes":[" умора ","вечерна\n почивка",""]}`)))
}

func TestHasMemoryThemes(t *testing.T) {
	assert.False(t, HasMemoryThemes(pqtype.NullRawMessage{}))
	assert.False(t, HasMemoryThemes(meta(`{"fact_key":"x"}`)))
	assert.True(t, HasMemoryThemes(meta(`{"themes":[]}`)))
	assert.True(t, HasMemoryThemes(meta(`{"themes":["умора"]}`)))
}

func TestWithoutMemoryThemes(t *testing.T) {
	assert.Equal(t, pqtype.NullRawMessage{}, WithoutMemoryThemes(pqtype.NullRawMessage{}))
	assert.Equal(t, map[string]any{"fact_key": "x"}, object(t, WithoutMemoryThemes(meta(`{"fact_key":"x","themes":["a"]}`))))
	assert.Equal(t, map[string]any{}, object(t, WithoutMemoryThemes(meta(`{"themes":["a"]}`))))
	assert.Equal(t, `{"fact_key":"x"}`, string(WithoutMemoryThemes(meta(`{"fact_key":"x"}`)).RawMessage), "untouched without themes")
}

func TestMemoryMetadataForUpdate(t *testing.T) {
	stored := meta(`{"fact_key":"rest","themes":["умора"]}`)
	none := pqtype.NullRawMessage{}

	t.Run("no metadata sent, same text: the query keeps the stored metadata", func(t *testing.T) {
		assert.Equal(t, none, MemoryMetadataForUpdate(none, stored, false))
	})
	t.Run("no metadata sent, new text: the stored metadata without the themes", func(t *testing.T) {
		assert.Equal(t, map[string]any{"fact_key": "rest"}, object(t, MemoryMetadataForUpdate(none, stored, true)))
	})
	t.Run("no metadata sent, new text, nothing to drop", func(t *testing.T) {
		assert.Equal(t, none, MemoryMetadataForUpdate(none, meta(`{"fact_key":"rest"}`), true))
		assert.Equal(t, none, MemoryMetadataForUpdate(none, none, true))
	})
	t.Run("metadata sent, same text: the stored themes stay, the client's are ignored", func(t *testing.T) {
		got := MemoryMetadataForUpdate(meta(`{"fact_key":"name","themes":["чужда"]}`), stored, false)
		assert.Equal(t, map[string]any{"fact_key": "name", "themes": []any{"умора"}}, object(t, got))
	})
	t.Run("metadata sent, new text: no themes", func(t *testing.T) {
		got := MemoryMetadataForUpdate(meta(`{"fact_key":"name","themes":["умора"]}`), stored, true)
		assert.Equal(t, map[string]any{"fact_key": "name"}, object(t, got))
	})
	t.Run("metadata sent, the stored memory has no themes", func(t *testing.T) {
		got := MemoryMetadataForUpdate(meta(`{"fact_key":"name","themes":["x"]}`), meta(`{}`), false)
		assert.Equal(t, map[string]any{"fact_key": "name"}, object(t, got))
	})
}
