package jobs

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	pgvector "github.com/pgvector/pgvector-go"
	"github.com/riverqueue/river"
	openai "github.com/sashabaranov/go-openai"
	"github.com/sqlc-dev/pqtype"

	"go-monolith/internal/config"
	"go-monolith/internal/db"
	"go-monolith/internal/llm"
	"go-monolith/metrics"
)

// MemoryThemesArgs is the periodic job of step 2.12: the local model gives every memory a few
// theme words - when it would help to recall it ("умора", "почивка", "вечер") - and the memory's
// vector is made again from "<themes>: <memory>" (db.MemoryEmbeddingText). Measured on 2026-11-22
// (measurements\memthemes_2026-11-22): the right memory came first for 29 of 37 personal messages
// instead of 24. Only while MEMORY_THEMES_ENABLED is on. A memory is written without themes and
// with a plain vector first, so no write path waits for the model; the job comes later.
type MemoryThemesArgs struct{}

func (MemoryThemesArgs) Kind() string { return "memory_themes" }

const (
	// memoryThemesInterval is how often the job looks for memories without themes.
	memoryThemesInterval = 5 * time.Minute
	// memoryThemesPerRun bounds the memories one run reads: one request to the model per person
	// among them (about 70 s for 30 memories on the local Gemma, measured).
	memoryThemesPerRun = 10
	// maxThemesPerMemory and maxThemeRunes bound what the model may add to a memory's vector.
	maxThemesPerMemory = 5
	maxThemeRunes      = 40
	// Results of a memory, the labels of the metric memory_themes_total.
	themesStored  = "themed"  // themes stored, the vector made from them
	themesNone    = "empty"   // the model gave no themes: marked as looked at, the vector stays
	themesChanged = "changed" // the text changed or the memory went while the model worked
	themesFailed  = "failed"  // the model or the embedder failed; the next run tries again
)

// memoryThemeCategories are the 13 categories of the quotes (measurements\quotes_ds_2026-11-22). The
// model names one or two for each memory, and they are not stored: asked with them, it writes themes
// of moods and moments (hit@1 28-29 of 37), without them themes of facts and misspelled words
// ("пройшла история", hit@1 26 of 37; measurements\memthemes_2026-11-22\variants.txt).
var memoryThemeCategories = []string{"умора и почивка", "работа и смени", "гняв и раздразнение", "тъга и самота",
	"близки и семейство", "здраве и тяло", "радост и добро настроение", "тревога и страх",
	"неуспех и загуба", "мотивация и цел", "време и промяна", "спокойствие и приемане", "бизнес и пари"}

// memoryThemesPrompt is the measured prompt of themes.py (2026-11-22).
var memoryThemesPrompt = `Ти помагаш на асистент да помни неща за един човек. За всеки номериран спомен напиши:
- "themes": 3 до 5 кратки думи или изрази на български — в какви моменти от деня или при какви чувства би било хубаво асистентът да се сети за този спомен (например за "почива на дивана пред телевизора": "умора", "почивка", "вечер", "отмора");
- "categories": 1 или 2 от тези категории (точно с тези имена): ` + strings.Join(memoryThemeCategories, ", ") + `.
Отговори само с JSON: {"items": [{"n": <номер>, "themes": [...], "categories": [...]}]}, по един запис за всеки спомен.`

// memoryThemesSchema is the JSON schema of the answer for n memories. The bounds matter: with a list
// of themes without maxItems the local Gemma repeated one theme ("пройшла история") until
// max_tokens in 3 answers of 4 for the same ten memories (2026-11-22).
func memoryThemesSchema(n int) json.RawMessage {
	raw, _ := json.Marshal(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"items": map[string]any{
				"type":     "array",
				"maxItems": n,
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"n": map[string]any{"type": "integer"},
						"themes": map[string]any{
							"type":     "array",
							"maxItems": maxThemesPerMemory,
							"items":    map[string]any{"type": "string", "maxLength": maxThemeRunes},
						},
						"categories": map[string]any{
							"type":     "array",
							"maxItems": 2,
							"items":    map[string]any{"type": "string", "enum": memoryThemeCategories},
						},
					},
					"required":             []string{"n", "themes", "categories"},
					"additionalProperties": false,
				},
			},
		},
		"required":             []string{"items"},
		"additionalProperties": false,
	})
	return raw
}

// memoryThemesAnswer is the model's answer.
type memoryThemesAnswer struct {
	Items []struct {
		N      int      `json:"n"`
		Themes []string `json:"themes"`
	} `json:"items"`
}

// MemoryThemesWorker gives memories their themes; see MemoryThemesArgs.
type MemoryThemesWorker struct {
	river.WorkerDefaults[MemoryThemesArgs]
	dbClient  *db.Database
	llmClient llm.LLMClient
	embedder  llm.Embedder
}

func NewMemoryThemesWorker(dbClient *db.Database, llmClient llm.LLMClient, embedder llm.Embedder) *MemoryThemesWorker {
	return &MemoryThemesWorker{dbClient: dbClient, llmClient: llmClient, embedder: embedder}
}

func (w *MemoryThemesWorker) Timeout(*river.Job[MemoryThemesArgs]) time.Duration {
	return 10 * time.Minute
}

// Work reads the next memories without themes and gives them themes, one request per person. A
// failure is logged and counted, and the job itself succeeds: the memories keep no themes and the
// next run, five minutes later, takes them again.
func (w *MemoryThemesWorker) Work(ctx context.Context, _ *river.Job[MemoryThemesArgs]) error {
	// Background work: the chat keeps its slot of the model.
	ctx = llm.WithBackground(ctx)
	rows, err := w.dbClient.Queries.GetMemoriesWithoutThemes(ctx, memoryThemesPerRun)
	if err != nil {
		return fmt.Errorf("failed to read the memories without themes: %w", err)
	}
	for start := 0; start < len(rows); {
		end := start + 1
		for end < len(rows) && rows[end].UserID == rows[start].UserID {
			end++
		}
		if ctx.Err() != nil {
			return nil
		}
		w.themePerson(ctx, rows[start:end])
		start = end
	}
	return nil
}

// themePerson asks the model for the themes of one person's memories and stores them.
func (w *MemoryThemesWorker) themePerson(ctx context.Context, rows []db.GetMemoriesWithoutThemesRow) {
	themes, err := w.askThemes(ctx, rows)
	if err != nil {
		slog.Error("the model gave no themes for the memories; the next run tries again",
			"user_id", rows[0].UserID, "memories", len(rows), "error", err)
		metrics.MemoryThemes.WithLabelValues(themesFailed).Add(float64(len(rows)))
		return
	}

	var texts []string
	var themed []int
	for i, row := range rows {
		if len(themes[i]) > 0 {
			texts = append(texts, db.MemoryEmbeddingText(row.Content, themesMetadata(themes[i])))
			themed = append(themed, i)
		}
	}
	vectors := map[int]pgvector.Vector{}
	if len(texts) > 0 {
		embedded, err := w.embedder.EmbedTexts(ctx, texts, false)
		if err == nil && len(embedded) != len(texts) {
			err = fmt.Errorf("the embedder returned %d vectors for %d memories", len(embedded), len(texts))
		}
		if err != nil {
			slog.Error("could not make the vectors of the memories from their themes; the next run tries again",
				"user_id", rows[0].UserID, "error", err)
			metrics.MemoryThemes.WithLabelValues(themesFailed).Add(float64(len(rows)))
			return
		}
		for k, i := range themed {
			if len(embedded[k]) != config.VectorDimension {
				slog.Error("a vector of the wrong size; the memory waits for the next run", "memory_id", rows[i].ID, "dim", len(embedded[k]))
				continue
			}
			vectors[i] = pgvector.NewVector(embedded[k])
		}
	}

	for i, row := range rows {
		params := db.SetMemoryThemesParams{ID: row.ID, UserID: row.UserID, ContentHash: row.ContentHash}
		if len(themes[i]) > 0 {
			vec, ok := vectors[i]
			if !ok {
				metrics.MemoryThemes.WithLabelValues(themesFailed).Inc()
				continue
			}
			params.Embedding = &vec
			params.EmbeddingModel = sql.NullString{String: config.DefaultEmbedModel, Valid: true}
		}
		raw, _ := json.Marshal(themes[i])
		params.Themes = raw
		n, err := w.dbClient.Queries.SetMemoryThemes(ctx, params)
		switch {
		case err != nil:
			slog.Error("could not store the themes of a memory", "memory_id", row.ID, "error", err)
			metrics.MemoryThemes.WithLabelValues(themesFailed).Inc()
		case n == 0:
			metrics.MemoryThemes.WithLabelValues(themesChanged).Inc()
		case len(themes[i]) == 0:
			metrics.MemoryThemes.WithLabelValues(themesNone).Inc()
		default:
			metrics.MemoryThemes.WithLabelValues(themesStored).Inc()
		}
	}
	slog.Info("memory themes", "user_id", rows[0].UserID, "memories", len(rows))
}

// themesMetadata is metadata with only the given themes, for db.MemoryEmbeddingText.
func themesMetadata(themes []string) pqtype.NullRawMessage {
	raw, _ := json.Marshal(map[string][]string{"themes": themes})
	return pqtype.NullRawMessage{RawMessage: raw, Valid: true}
}

// askThemes returns the cleaned themes of each memory, in the order of rows (an empty slice for a
// memory the model gave none). An error means no answer to use: the memories wait for the next run.
func (w *MemoryThemesWorker) askThemes(ctx context.Context, rows []db.GetMemoriesWithoutThemesRow) ([][]string, error) {
	var list strings.Builder
	for i, row := range rows {
		fmt.Fprintf(&list, "%d. %s\n", i+1, strings.Join(strings.Fields(row.Content), " "))
	}
	reqCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	resp, err := w.llmClient.ChatWithJSONSchema(reqCtx, openai.ChatCompletionRequest{
		Messages: []openai.ChatCompletionMessage{
			{Role: openai.ChatMessageRoleSystem, Content: memoryThemesPrompt},
			{Role: openai.ChatMessageRoleUser, Content: list.String()},
		},
		//lint:ignore SA1019 llama-server reads max_tokens; max_completion_tokens is OpenAI's own name.
		MaxTokens:   200 + 120*len(rows),
		Temperature: 0.2,
	}, memoryThemesSchema(len(rows)))
	if err != nil {
		return nil, err
	}
	if len(resp.Choices) == 0 {
		return nil, fmt.Errorf("the model gave no answer")
	}
	content := strings.TrimSpace(resp.Choices[0].Message.Content)
	content = strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(content, "```json"), "```"), "```")
	var answer memoryThemesAnswer
	if err := json.Unmarshal([]byte(strings.TrimSpace(content)), &answer); err != nil {
		return nil, fmt.Errorf("the answer is not the JSON asked for (finish reason %q, %d characters): %w",
			resp.Choices[0].FinishReason, len(content), err)
	}
	out := make([][]string, len(rows))
	for i := range out {
		out[i] = []string{}
	}
	for _, item := range answer.Items {
		if item.N < 1 || item.N > len(rows) || len(out[item.N-1]) > 0 {
			continue
		}
		out[item.N-1] = cleanThemes(item.Themes)
	}
	return out, nil
}

// cleanThemes keeps at most maxThemesPerMemory distinct short themes, one line each.
func cleanThemes(themes []string) []string {
	out := []string{}
	seen := map[string]bool{}
	for _, t := range themes {
		t = strings.Trim(strings.Join(strings.Fields(t), " "), " .,;:\"'„“")
		key := strings.ToLower(t)
		if t == "" || seen[key] || utf8.RuneCountInString(t) > maxThemeRunes {
			continue
		}
		seen[key] = true
		out = append(out, t)
		if len(out) == maxThemesPerMemory {
			break
		}
	}
	return out
}

// memoryThemesJob is the periodic job, registered only while MEMORY_THEMES_ENABLED is on.
func memoryThemesJob() *river.PeriodicJob {
	return river.NewPeriodicJob(
		river.PeriodicInterval(memoryThemesInterval),
		func() (river.JobArgs, *river.InsertOpts) {
			return MemoryThemesArgs{}, &river.InsertOpts{
				Queue: "memory",
				UniqueOpts: river.UniqueOpts{
					ByArgs:  true,
					ByState: uniqueWhileInFlightStates(),
				},
			}
		},
		nil,
	)
}
