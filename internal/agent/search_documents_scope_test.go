package agent

import (
	"context"
	"testing"

	"github.com/google/uuid"
	pgvector "github.com/pgvector/pgvector-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go-monolith/internal/config"
	"go-monolith/internal/db"
	"go-monolith/internal/llm"
	"go-monolith/internal/retrieval"
)

// scopeLibrary puts two files into the user's library: CLBSV2.pdf with five passages and
// freud.txt with six passages that are all nearer to the question than any of them (they are
// the question itself, which the mock embedder puts at distance 0).
func scopeLibrary(t *testing.T, e *toolEnv, question string) {
	t.Helper()
	ctx := context.Background()
	add := func(name string, texts []string) {
		file, err := e.db.Queries.CreateIngestedFile(ctx, db.CreateIngestedFileParams{
			UserID: e.userID, FileName: name, FileHash: "hash-" + uuid.NewString(),
			MimeType: "application/pdf", FileSize: 1, Status: "completed", LocalPath: "storage/none",
		})
		require.NoError(t, err)
		vectors, err := (&llm.MockEmbedder{}).EmbedTexts(ctx, texts, false)
		require.NoError(t, err)
		var arg db.CreateDocumentChunksBatchParams
		arg.EmbeddingModel = config.DefaultEmbedModel
		for i, text := range texts {
			arg.FileIds = append(arg.FileIds, file.ID)
			arg.UserIds = append(arg.UserIds, e.userID)
			arg.ChunkIndices = append(arg.ChunkIndices, int32(i))
			arg.Texts = append(arg.Texts, text)
			arg.Embeddings = append(arg.Embeddings, pgvector.NewVector(vectors[i]))
			arg.ChunkTypes = append(arg.ChunkTypes, "raw_text")
			arg.Metadatas = append(arg.Metadatas, "{}")
		}
		require.NoError(t, e.db.Queries.CreateDocumentChunksBatch(ctx, arg))
	}
	add("CLBSV2.pdf", []string{"clbs фаза едно", "clbs фаза две", "clbs фаза три", "clbs фаза четири", "clbs фаза пет"})
	add("freud.txt", []string{question, question + " 1", question + " 2", question + " 3", question + " 4", question + " 5"})
}

func scopeFiles(t *testing.T, res interface{}) []string {
	t.Helper()
	var files []string
	for _, r := range resultMap(t, res)["results"].([]map[string]interface{}) {
		files = append(files, r["file_name"].(string))
	}
	return files
}

// A question that names a file of the library is about that file (2026-11-15: "какво пише във
// файла CLBSV2" showed 2 passages of it and 6 of Freud, a cookbook and a book about vitamin B12,
// and the sources panel listed them all).
func TestSearchDocumentsStaysInsideTheFileTheQuestionNames(t *testing.T) {
	e := newToolEnv(t)
	const question = "какво пише във файла CLBSV2"
	scopeLibrary(t, e, question)
	ctx, turn := e.turn("s", false)
	ctx = WithCurrentQuery(ctx, question)

	res, err := e.reg.ExecuteTool(ctx, "search_documents", map[string]interface{}{"query": question})
	require.NoError(t, err)

	files := scopeFiles(t, res)
	assert.Len(t, files, 5, "all the passages of the file, not two")
	for _, f := range files {
		assert.Equal(t, "CLBSV2.pdf", f)
	}
	assert.Equal(t, "CLBSV2.pdf", resultMap(t, res)["searched_only_in_file"], "the model is told the search was inside the file")
	for _, s := range turn.foundSources() {
		assert.Equal(t, "CLBSV2.pdf", s.FileName, "and the sources panel lists nothing else")
	}
	assert.Len(t, turn.foundSources(), 5)
}

func TestSearchDocumentsSearchesTheWholeLibraryWhenNoFileIsNamed(t *testing.T) {
	e := newToolEnv(t)
	const question = "какво пише в книгите за фазите на пазара"
	scopeLibrary(t, e, question)
	ctx, _ := e.turn("s", false)
	ctx = WithCurrentQuery(ctx, question)

	res, err := e.reg.ExecuteTool(ctx, "search_documents", map[string]interface{}{"query": question})
	require.NoError(t, err)

	assert.Contains(t, scopeFiles(t, res), "freud.txt", "the whole library, as before")
	assert.NotContains(t, resultMap(t, res), "searched_only_in_file")
}

// The model's own query can name the file when the user's message does not ("и какво още има
// в него?" after the file was named earlier is not covered: only this turn's texts are read).
func TestSearchDocumentsReadsTheFileNameFromTheModelsQueryToo(t *testing.T) {
	e := newToolEnv(t)
	const question = "какво пише в него"
	scopeLibrary(t, e, question)
	ctx, _ := e.turn("s", false)
	ctx = WithCurrentQuery(ctx, question)

	res, err := e.reg.ExecuteTool(ctx, "search_documents", map[string]interface{}{"query": "CLBSV2.pdf съдържание"})
	require.NoError(t, err)

	for _, f := range scopeFiles(t, res) {
		assert.Equal(t, "CLBSV2.pdf", f)
	}
	assert.Equal(t, "CLBSV2.pdf", resultMap(t, res)["searched_only_in_file"])
}

// The English query of the model is searched inside the file too, and the passages of both
// searches are merged as before.
func TestSearchDocumentsSearchesTheEnglishQueryInsideTheFileToo(t *testing.T) {
	e := newToolEnv(t)
	log := &embedderLog{}
	reg := NewDefaultToolRegistry(retrieval.NewRetrievalService(e.db, log), e.db, e.svc)
	const question, english = "какво пише във файла CLBSV2", "what is in the file CLBSV2"
	scopeLibrary(t, e, question)
	ctx, _ := e.turn("s", false)
	ctx = WithCurrentQuery(ctx, question)

	res, err := reg.ExecuteTool(ctx, "search_documents", map[string]interface{}{"query": question, "query_en": english})
	require.NoError(t, err)

	assert.Equal(t, []string{question, english}, log.seen())
	for _, f := range scopeFiles(t, res) {
		assert.Equal(t, "CLBSV2.pdf", f)
	}
}

// The model's query is usually about the subject ("фази на пазара") and the file is named only in the
// user's message: that is enough.
func TestSearchDocumentsReadsTheFileNameFromTheUsersMessageToo(t *testing.T) {
	e := newToolEnv(t)
	const question = "какво пише във файла CLBSV2"
	scopeLibrary(t, e, "фази на пазара")
	ctx, _ := e.turn("s", false)
	ctx = WithCurrentQuery(ctx, question)

	res, err := e.reg.ExecuteTool(ctx, "search_documents", map[string]interface{}{"query": "фази на пазара"})
	require.NoError(t, err)

	for _, f := range scopeFiles(t, res) {
		assert.Equal(t, "CLBSV2.pdf", f)
	}
	assert.Equal(t, "CLBSV2.pdf", resultMap(t, res)["searched_only_in_file"])
}

// "CLBSV2" is six letters, and a query of fewer than seven without a question word is filler: the
// model sent exactly that when it was asked what is in the file, and got "skipped" and nothing.
func TestSearchDocumentsDoesNotSkipAShortQueryThatIsTheNameOfTheNamedFile(t *testing.T) {
	e := newToolEnv(t)
	scopeLibrary(t, e, "фази на пазара")
	ctx, _ := e.turn("s", false)

	short, err := e.reg.ExecuteTool(WithCurrentQuery(ctx, "какво пише във файла CLBSV2"), "search_documents",
		map[string]interface{}{"query": "CLBSV2", "query_en": "CLBSV2"})
	require.NoError(t, err)
	assert.Equal(t, "success", resultMap(t, short)["status"])
	assert.Len(t, scopeFiles(t, short), 5)

	filler, err := e.reg.ExecuteTool(WithCurrentQuery(ctx, "какво пише във файла CLBSV2"), "search_documents",
		map[string]interface{}{"query": "CLBSV2"})
	require.NoError(t, err)
	assert.Equal(t, "success", resultMap(t, filler)["status"])

	// With no file named, a short query is filler as before.
	skipped, err := e.reg.ExecuteTool(WithCurrentQuery(ctx, "здрасти"), "search_documents", map[string]interface{}{"query": "здрасти"})
	require.NoError(t, err)
	assert.Equal(t, "skipped", resultMap(t, skipped)["status"])
}
