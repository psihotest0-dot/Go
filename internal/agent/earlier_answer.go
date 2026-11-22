package agent

import (
	"context"
	"encoding/json"
	"log/slog"
	"regexp"
	"strings"

	"github.com/google/uuid"

	"go-monolith/internal/api"
	"go-monolith/internal/db"
	"go-monolith/internal/retrieval"
)

// "А какво още има в нея?" after an answer from a book: the message does not name the book, the
// model sends a query as vague as the message, and the search of the whole library brings
// passages of other books (plan step 1.1, the idea I1 from the review of AnythingLLM, 2026-11-22).
// The answers before keep their sources (messages.source_chunk_ids), so the turn knows which
// book "нея" is.

// earlierAnswerTurns is how many of the last answers are looked at for sources.
const earlierAnswerTurns = 2

// minFoundPassages: when the book of the earlier answer has fewer new passages than this, the
// passages of that answer are shown too.
const minFoundPassages = 3

// earlierAnswer is the last answer of the conversation that rested on passages of the library:
// the question it answered and its sources (chunk ids; a memory among them matches no chunk).
type earlierAnswer struct {
	question string
	chunkIDs []uuid.UUID
}

// asksForMore: the message asks to go on with what was said - "а какво още има в нея", "разкажи
// още", "продължи", "what else".
var asksForMore = regexp.MustCompile(`(?i)(?:^|[^\p{L}])(?:` +
	`(?:какво|какви|кои|нещо|има\s+ли)\s+още|още\s+(?:нещо|за|от|по|малко)|` +
	`(?:разкажи|кажи|дай|пиши|напиши|обясни|цитирай)(?:\s+ми)?\s+още|` +
	`продължи|продължавай|какво\s+друго|друго\s+(?:има|пише|казва)|` +
	`what\s+else|tell\s+me\s+more|more\s+(?:about|from)|go\s+on|continue|keep\s+going` +
	`)(?:[^\p{L}]|$)`)

// earlierAnswerOf finds the earlier answer in the history (the newest first; its first message of
// the person is the one of this turn): the newest of the last earlierAnswerTurns answers that has
// sources, and the message of the person it answered.
func earlierAnswerOf(history []db.Message) earlierAnswer {
	answers := 0
	for i, m := range history {
		if m.Role != "assistant" {
			continue
		}
		answers++
		if answers > earlierAnswerTurns {
			break
		}
		ids := chunkIDsOf(m)
		if len(ids) == 0 {
			continue
		}
		found := earlierAnswer{chunkIDs: ids}
		for _, before := range history[i+1:] {
			if before.Role == "user" {
				found.question = before.Content
				break
			}
		}
		return found
	}
	return earlierAnswer{}
}

// chunkIDsOf are the sources of a stored answer, in their order, each once.
func chunkIDsOf(m db.Message) []uuid.UUID {
	if !m.SourceChunkIds.Valid {
		return nil
	}
	var stored []api.DBChunkInfo
	if json.Unmarshal(m.SourceChunkIds.RawMessage, &stored) != nil {
		return nil
	}
	ids := make([]uuid.UUID, 0, len(stored))
	seen := map[uuid.UUID]bool{}
	for _, s := range stored {
		if s.ChunkID != uuid.Nil && !seen[s.ChunkID] {
			seen[s.ChunkID] = true
			ids = append(ids, s.ChunkID)
		}
	}
	return ids
}

// setEarlierAnswer records the earlier answer of the conversation for the tools of the turn.
func (t *turnState) setEarlierAnswer(a earlierAnswer) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.earlier = a
}

// earlierAnswerOfTurn is the earlier answer recorded by setEarlierAnswer.
func (t *turnState) earlierAnswerOfTurn() earlierAnswer {
	if t == nil {
		return earlierAnswer{}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.earlier
}

// earlierPassage is a passage of the earlier answer as the model gets it: marked, so that it is
// not taken for something new.
type earlierPassage struct {
	ID       uuid.UUID
	Content  string
	FileName string
}

// withEarlierAnswer is the search of search_documents for a message that asks for more after an
// answer from the library: the search goes inside the book of the earlier answer, with the
// earlier question and the model's query, and leaves out the passages that answer already used.
// What it returns are the passages the model gets as new (the sources panel shows them), and,
// when the book had fewer than minFoundPassages new ones, the passages of the earlier answer
// marked as such (the panel does not show them: they are not a source of this answer, R5).
//
// Measured on the 43 book questions of task 32, each followed by "А какво още има в нея?", with
// the real model (measurements\earlier_2026-11-22): before, the follow-up got 8 passages of 4-5
// books, 1.2 of them new passages of the book; adding the earlier passages to that (the plan's
// first form) changed nothing in what was found. Searched inside the book: 8 of 8 new passages of
// it, in 43 of 43; the right book (the one task 32 expects) in 41 of 43.
//
// A message that does not ask for more is left as it was: filling a search that found little with
// the passages of the earlier answer was not measured, and for a new topic it would bring the old
// book back.
func (r *DefaultToolRegistry) withEarlierAnswer(ctx context.Context, userID uuid.UUID, query string, scoped bool, limit int,
	results []retrieval.SearchResult) ([]retrieval.SearchResult, []earlierPassage) {
	earlier := turnFrom(ctx).earlierAnswerOfTurn()
	if len(earlier.chunkIDs) == 0 || scoped || r.db == nil || !asksForMore.MatchString(currentQuery(ctx)) {
		return results, nil
	}
	chunks, err := r.db.Queries.GetDocumentChunksWithFile(ctx, db.GetDocumentChunksWithFileParams{ChunkIds: earlier.chunkIDs, UserID: userID})
	if err != nil {
		slog.WarnContext(ctx, "the passages of the earlier answer could not be read", "error", err)
		return results, nil
	}
	// The rows come in no order; the sources of the answer are in the order the search ranked them.
	byID := make(map[uuid.UUID]db.GetDocumentChunksWithFileRow, len(chunks))
	for _, c := range chunks {
		byID[c.ID] = c
	}
	shown := map[uuid.UUID]bool{}
	var passages []earlierPassage
	files := map[uuid.UUID]int{}
	var order []uuid.UUID
	for _, id := range earlier.chunkIDs {
		c, ok := byID[id]
		if !ok {
			continue
		}
		shown[c.ID] = true
		if c.ChunkType == "concept_summary" {
			continue
		}
		passages = append(passages, earlierPassage{ID: c.ID, Content: c.Text, FileName: c.FileName})
		if files[c.FileID] == 0 {
			order = append(order, c.FileID)
		}
		files[c.FileID]++
	}

	// The book of the earlier answer: the one most of its passages came from, and of two with as
	// many the one ranked first. The search gives at most two passages of a book, so three books
	// with two each are common; chosen by the count alone (in no order), the book was the right
	// one for 35 of 42 questions of task 32, by the count and then the rank for 41.
	var book uuid.UUID
	for _, f := range order {
		if files[f] > files[book] {
			book = f
		}
	}
	if book == uuid.Nil {
		return results, nil
	}
	var fresh []retrieval.SearchResult
	for _, q := range []string{earlier.question, query} {
		if strings.TrimSpace(q) == "" {
			continue
		}
		found, err := r.retrieval.SearchFileChunks(ctx, userID, book, q, limit+len(shown))
		if err != nil {
			slog.WarnContext(ctx, "the book of the earlier answer could not be searched", "error", err)
			continue
		}
		var unseen []retrieval.SearchResult
		for _, f := range found {
			if !shown[f.ID] {
				unseen = append(unseen, f)
			}
		}
		fresh = mergePassages(limit, fresh, unseen)
	}
	switch {
	case len(fresh) == 0:
		// Nothing new in the book (or it could not be searched): the search of the library stands,
		// and the model is shown what it already said.
		return results, without(passages, results)
	case len(fresh) < minFoundPassages:
		return fresh, without(passages, fresh)
	}
	return fresh, nil
}

// without are the passages that are not among the results already.
func without(passages []earlierPassage, results []retrieval.SearchResult) []earlierPassage {
	in := make(map[uuid.UUID]bool, len(results))
	for _, r := range results {
		in[r.ID] = true
	}
	var out []earlierPassage
	for _, p := range passages {
		if !in[p.ID] {
			out = append(out, p)
		}
	}
	return out
}

// earlierPassagesNote tells the model what the marked passages are.
const earlierPassagesNote = "The results marked from_earlier_answer are passages you already used in your earlier answer in this conversation. " +
	"They show what the conversation is about; do not present them as something new."
