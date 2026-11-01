package ingest

import "testing"

// The vector of a stored chunk can be rebuilt only if its text is built the same
// way as when it was ingested, so the format is pinned.
func TestEmbeddingText(t *testing.T) {
	tests := []struct {
		name      string
		chunkType string
		file      string
		index     int
		text      string
		want      string
	}{
		{"raw chunk carries its file and position", ChunkTypeRaw, "Примерна книга.epub", 12, "Парите са средство.", "[Файл: Примерна книга.epub - Chunk 12] Парите са средство."},
		{"first chunk", ChunkTypeRaw, "a.txt", 0, "x", "[Файл: a.txt - Chunk 0] x"},
		{"a summary is embedded as it is", ChunkTypeConcept, "Примерна книга.epub", 2400, "Обобщение на темата.", "Обобщение на темата."},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := EmbeddingText(tt.chunkType, tt.file, tt.index, tt.text); got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}
