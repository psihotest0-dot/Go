package ingest

import "fmt"

// ChunkTypeRaw and ChunkTypeConcept are the values of document_chunks.chunk_type.
const (
	ChunkTypeRaw     = "raw_text"
	ChunkTypeConcept = "concept_summary"
)

// EmbeddingText is the text that is embedded for a stored chunk: a raw chunk
// carries its file name and position in front, so that a hit on "Chunk 12 of
// Примерна книга" is not a stranger to the model; a concept summary is embedded as it
// is. Ingest and cmd/reembed both use it - the vector of a chunk can be rebuilt
// only if the text is built the same way both times.
func EmbeddingText(chunkType, fileName string, chunkIndex int, text string) string {
	if chunkType == ChunkTypeConcept {
		return text
	}
	return fmt.Sprintf("[Файл: %s - Chunk %d] %s", fileName, chunkIndex, text)
}
