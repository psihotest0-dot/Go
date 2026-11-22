package jobs

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	pgvector "github.com/pgvector/pgvector-go"
	"github.com/riverqueue/river"
	openai "github.com/sashabaranov/go-openai"
	"github.com/sqlc-dev/pqtype"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go-monolith/internal/config"
	"go-monolith/internal/db"
	"go-monolith/internal/dbtest"
	"go-monolith/internal/llm"
)

// themesLLM answers the theme requests with answer(userMessage) and remembers the requests.
type themesLLM struct {
	mu       sync.Mutex
	answer   func(list string) (string, error)
	requests []string
}

func (m *themesLLM) Chat(context.Context, openai.ChatCompletionRequest) (openai.ChatCompletionResponse, error) {
	return openai.ChatCompletionResponse{}, errors.New("not used")
}

func (m *themesLLM) ChatStream(context.Context, openai.ChatCompletionRequest) (llm.Stream, error) {
	return nil, errors.New("not used")
}

func (m *themesLLM) ChatWithJSONSchema(ctx context.Context, req openai.ChatCompletionRequest, _ interface{}) (openai.ChatCompletionResponse, error) {
	m.mu.Lock()
	list := req.Messages[len(req.Messages)-1].Content
	m.requests = append(m.requests, list)
	m.mu.Unlock()
	if !llm.IsBackground(ctx) {
		return openai.ChatCompletionResponse{}, errors.New("the theme job must be background work")
	}
	content, err := m.answer(list)
	if err != nil {
		return openai.ChatCompletionResponse{}, err
	}
	return openai.ChatCompletionResponse{Choices: []openai.ChatCompletionChoice{{Message: openai.ChatCompletionMessage{Content: content}}}}, nil
}

// textEmbedder gives every text a vector whose first number is its length, and remembers the texts.
type textEmbedder struct {
	mu    sync.Mutex
	texts []string
}

func (e *textEmbedder) EmbedTexts(_ context.Context, texts []string, _ bool) ([][]float32, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.texts = append(e.texts, texts...)
	out := make([][]float32, len(texts))
	for i, t := range texts {
		out[i] = make([]float32, config.VectorDimension)
		out[i][0] = float32(len([]rune(t)))
	}
	return out, nil
}

type themesEnv struct {
	t        *testing.T
	ctx      context.Context
	database *db.Database
	llm      *themesLLM
	embedder *textEmbedder
	worker   *MemoryThemesWorker
}

func newThemesEnv(t *testing.T, answer func(list string) (string, error)) *themesEnv {
	t.Helper()
	ctx := context.Background()
	database, err := db.Connect(ctx, dbtest.ConnString(t))
	require.NoError(t, err)
	t.Cleanup(database.Close)
	m := &themesLLM{answer: answer}
	e := &textEmbedder{}
	return &themesEnv{t: t, ctx: ctx, database: database, llm: m, embedder: e, worker: NewMemoryThemesWorker(database, m, e)}
}

func (e *themesEnv) user() uuid.UUID {
	e.t.Helper()
	u, err := e.database.Queries.CreateUser(e.ctx)
	require.NoError(e.t, err)
	e.t.Cleanup(func() { dbtest.DeleteUser(e.t, e.database.Pool, u.ID) })
	return u.ID
}

// memory stores a memory as a write path does: no themes, a plain vector (first number 1).
func (e *themesEnv) memory(user uuid.UUID, content, metadata string) uuid.UUID {
	e.t.Helper()
	vec := make([]float32, config.VectorDimension)
	vec[0] = 1
	meta := pqtype.NullRawMessage{}
	if metadata != "" {
		meta = pqtype.NullRawMessage{RawMessage: []byte(metadata), Valid: true}
	}
	m, err := e.database.Queries.CreateMemory(e.ctx, db.CreateMemoryParams{
		UserID: user, Content: content, Status: "active", Embedding: pgvector.NewVector(vec),
		EmbeddingModel: config.DefaultEmbedModel, Importance: 5, Metadata: meta, ContentHash: db.GenerateHash(content),
	})
	require.NoError(e.t, err)
	return m.ID
}

func (e *themesEnv) run() {
	e.t.Helper()
	require.NoError(e.t, e.worker.Work(e.ctx, &river.Job[MemoryThemesArgs]{}))
}

// state is the stored metadata and the first number of the vector of a memory.
func (e *themesEnv) state(user, id uuid.UUID) (map[string]any, float32) {
	e.t.Helper()
	m, err := e.database.Queries.GetMemoryByID(e.ctx, db.GetMemoryByIDParams{ID: id, UserID: user})
	require.NoError(e.t, err)
	var meta map[string]any
	if m.Metadata.Valid {
		require.NoError(e.t, json.Unmarshal(m.Metadata.RawMessage, &meta))
	}
	return meta, m.Embedding.Slice()[0]
}

// answerFor answers each numbered line of the request with themes(line).
func answerFor(themes func(memory string) []string) func(string) (string, error) {
	return func(list string) (string, error) {
		type item struct {
			N      int      `json:"n"`
			Themes []string `json:"themes"`
		}
		var items []item
		for _, line := range strings.Split(strings.TrimSpace(list), "\n") {
			var n int
			var text string
			if _, err := fmt.Sscanf(line, "%d.", &n); err != nil {
				return "", err
			}
			text = strings.TrimSpace(line[strings.Index(line, ".")+1:])
			items = append(items, item{N: n, Themes: themes(text)})
		}
		raw, err := json.Marshal(map[string]any{"items": items})
		return string(raw), err
	}
}

// The test database is shared: the job reads every person's memories without themes, so the tests
// look only at their own people's memories and requests.
func requestsWith(m *themesLLM, text string) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for _, r := range m.requests {
		if strings.Contains(r, text) {
			out = append(out, r)
		}
	}
	return out
}

func TestTheJobGivesAMemoryThemesAndAVectorFromThem(t *testing.T) {
	const sofa = "Ивайло почива на дивана пред телевизора."
	e := newThemesEnv(t, answerFor(func(memory string) []string {
		if memory == sofa {
			return []string{" умора", "почивка", "Умора", "вечер.", strings.Repeat("я", 41), "отмора", "диван", "шесто"}
		}
		return nil
	}))
	user := e.user()
	sofaID := e.memory(user, sofa, `{"fact_key":"rest"}`)
	bareID := e.memory(user, "Ивайло ползва Go.", "")

	for i := 0; i < 5 && len(requestsWith(e.llm, sofa)) == 0; i++ {
		e.run() // other people's memories in the shared database may come first
	}
	require.Len(t, requestsWith(e.llm, sofa), 1)
	assert.Contains(t, requestsWith(e.llm, sofa)[0], "Ивайло ползва Go.", "one request for the memories of one person")

	meta, first := e.state(user, sofaID)
	assert.Equal(t, map[string]any{"fact_key": "rest", "themes": []any{"умора", "почивка", "вечер", "отмора", "диван"}}, meta,
		"cleaned: trimmed, no repeats, nothing too long, at most five; the other metadata stays")
	text := "умора, почивка, вечер, отмора, диван: " + sofa
	assert.Contains(t, e.embedder.texts, text)
	assert.EqualValues(t, len([]rune(text)), first, "the vector is made from themes and text")

	meta, first = e.state(user, bareID)
	assert.Equal(t, map[string]any{"themes": []any{}}, meta, "no themes: looked at, so not asked again")
	assert.EqualValues(t, 1, first, "and the vector stays")

	e.run()
	assert.Len(t, requestsWith(e.llm, sofa), 1, "a memory that was looked at is not asked about again")
}

func TestAFailedModelLeavesTheMemoriesForTheNextRun(t *testing.T) {
	const back = "Ивайло го боли кръстът от седене."
	fail := true
	e := newThemesEnv(t, func(list string) (string, error) {
		if strings.Contains(list, back) && fail {
			return "", errors.New("lemonade is down")
		}
		return answerFor(func(string) []string { return []string{"болка", "здраве"} })(list)
	})
	user := e.user()
	id := e.memory(user, back, "")

	for i := 0; i < 5 && len(requestsWith(e.llm, back)) == 0; i++ {
		e.run()
	}
	meta, first := e.state(user, id)
	assert.Nil(t, meta, "nothing stored")
	assert.EqualValues(t, 1, first)

	fail = false
	for i := 0; i < 5 && len(requestsWith(e.llm, back)) < 2; i++ {
		e.run()
	}
	meta, _ = e.state(user, id)
	assert.Equal(t, map[string]any{"themes": []any{"болка", "здраве"}}, meta, "the next run takes it again")
}

func TestAnAnswerThatIsNotJSONStoresNothing(t *testing.T) {
	const tea = "Ивайло пие чай сутрин."
	e := newThemesEnv(t, func(list string) (string, error) {
		if strings.Contains(list, tea) {
			return "умора, почивка", nil
		}
		return answerFor(func(string) []string { return nil })(list)
	})
	user := e.user()
	id := e.memory(user, tea, "")
	for i := 0; i < 5 && len(requestsWith(e.llm, tea)) == 0; i++ {
		e.run()
	}
	require.NotEmpty(t, requestsWith(e.llm, tea))
	meta, _ := e.state(user, id)
	assert.Nil(t, meta, "not marked: the next run asks again")
}

// The person edits the memory while the model works: the themes of the old text must not land on
// the new one.
func TestThemesOfAnOldTextAreNotStored(t *testing.T) {
	const old = "Ивайло работи нощни смени."
	var e *themesEnv
	var user, id uuid.UUID
	e = newThemesEnv(t, func(list string) (string, error) {
		if strings.Contains(list, old) {
			_, err := e.database.UpdateMemoryWithCheck(e.ctx, db.UpdateMemoryParams{
				ID: id, UserID: user, Content: sqlString("Ивайло работи дневни смени."),
			})
			if err != nil {
				return "", err
			}
		}
		return answerFor(func(string) []string { return []string{"нощ", "умора"} })(list)
	})
	user = e.user()
	id = e.memory(user, old, "")
	for i := 0; i < 5 && len(requestsWith(e.llm, old)) == 0; i++ {
		e.run()
	}
	require.Len(t, requestsWith(e.llm, old), 1)
	meta, first := e.state(user, id)
	assert.Nil(t, meta, "the new text has no themes yet")
	assert.EqualValues(t, 1, first)

	for i := 0; i < 5 && len(requestsWith(e.llm, "дневни смени")) == 0; i++ {
		e.run()
	}
	meta, _ = e.state(user, id)
	assert.Equal(t, map[string]any{"themes": []any{"нощ", "умора"}}, meta, "the next run takes the new text")
}

func TestOneRequestHoldsTheMemoriesOfOnePerson(t *testing.T) {
	e := newThemesEnv(t, answerFor(func(string) []string { return []string{"тема"} }))
	anna, boris := e.user(), e.user()
	e.memory(anna, "Анна обича котки.", "")
	e.memory(boris, "Борис кара колело.", "")
	for i := 0; i < 5 && (len(requestsWith(e.llm, "Анна обича котки.")) == 0 || len(requestsWith(e.llm, "Борис кара колело.")) == 0); i++ {
		e.run()
	}
	for _, r := range requestsWith(e.llm, "Анна обича котки.") {
		assert.NotContains(t, r, "Борис")
	}
	require.NotEmpty(t, requestsWith(e.llm, "Борис кара колело."))
}

func TestCleanThemes(t *testing.T) {
	assert.Equal(t, []string{}, cleanThemes(nil))
	assert.Equal(t, []string{"умора", "лоша нощ"}, cleanThemes([]string{"„умора“", "лоша\tнощ", "  ", "УМОРА"}))
}

func sqlString(s string) sql.NullString { return sql.NullString{String: s, Valid: true} }
