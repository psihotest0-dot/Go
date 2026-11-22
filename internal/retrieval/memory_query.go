package retrieval

import (
	"context"
	"log/slog"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
)

// A short message - "а как да се справя?", "дано е минала", "особено баницата" - says little
// about what the conversation is about, and the memories it finds alone are mostly the wrong
// ones. So for a short message the memories are searched twice: with the message alone and with
// the message and the person's last earlier messages, and the two lists are taken in turn (plan
// step 0.5, I4, 2026-11-22).
//
// Measured (measurements\memquery_2026-11-22: the 30 invented memories of p55_fixture, the real
// embedder, 24 short conversations with a known memory): on the topic of the earlier messages
// the memory was in the first 3 for 6 of 20 with the message alone, 20 of 20 with the earlier
// messages; but where the short message changes the topic ("а котката?" after talk of the bot)
// the earlier messages pushed it out (3 of 4, against 4 of 4). In turn: 20 of 20 and 4 of 4. The
// conversations were written for the test, so 20 of 20 is the best case, not what live talk gives.

const (
	// shortMessageRunes: a message shorter than this gets the earlier messages in its query.
	shortMessageRunes = 40
	// EarlierMessagesForMemories is how many earlier messages of the person are used.
	EarlierMessagesForMemories = 3
	// maxEarlierMessageRunes bounds one earlier message in the query, so that a long one does
	// not decide alone what the short message is about.
	maxEarlierMessageRunes = 300
)

// conversationQuery is the query with the earlier messages, or "" when the message is not short or
// there is nothing earlier. earlier is the most recent first.
func conversationQuery(message string, earlier []string) string {
	message = strings.TrimSpace(message)
	if message == "" || utf8.RuneCountInString(message) >= shortMessageRunes {
		return ""
	}
	parts := []string{message}
	for _, e := range earlier {
		if len(parts) > EarlierMessagesForMemories {
			break
		}
		e = strings.Join(strings.Fields(e), " ")
		if e == "" {
			continue
		}
		if r := []rune(e); len(r) > maxEarlierMessageRunes {
			e = string(r[:maxEarlierMessageRunes])
		}
		parts = append(parts, e)
	}
	if len(parts) == 1 {
		return ""
	}
	return strings.Join(parts, "\n")
}

// inTurn takes the results of two searches in turn - the first of a, the first of b, the second
// of a... - each memory once, at most limit.
func inTurn(a, b []SearchResult, limit int) []SearchResult {
	out := make([]SearchResult, 0, limit)
	seen := make(map[uuid.UUID]bool, limit)
	for i := 0; len(out) < limit && (i < len(a) || i < len(b)); i++ {
		for _, list := range [][]SearchResult{a, b} {
			if i < len(list) && !seen[list[i].ID] && len(out) < limit {
				seen[list[i].ID] = true
				out = append(out, list[i])
			}
		}
	}
	return out
}

// memoriesInConversation are the memories for the message: those of the message alone, and for a
// short message those of the message with the earlier ones, in turn. queryVec is the vector of the
// message (nil when the embedder is down: then only the text half searches, and the message alone).
func (s *RetrievalService) memoriesInConversation(ctx context.Context, userID uuid.UUID, message string, earlier []string, limit int, queryVec []float32) ([]SearchResult, error) {
	alone, err := s.searchMemories(ctx, userID, message, limit, queryVec)
	if err != nil {
		return nil, err
	}
	joined := conversationQuery(message, earlier)
	if joined == "" || queryVec == nil {
		return alone, nil
	}
	joinedVec, err := s.embedQuery(ctx, joined)
	if err != nil || joinedVec == nil {
		// The memories of the message alone are still an answer.
		slog.WarnContext(ctx, "the query of the memories with the earlier messages could not be embedded; the message alone is used", "error", err)
		return alone, nil
	}
	withEarlier, err := s.searchMemories(ctx, userID, joined, limit, joinedVec)
	if err != nil {
		slog.WarnContext(ctx, "the memories with the earlier messages could not be searched; the message alone is used", "error", err)
		return alone, nil
	}
	return inTurn(alone, withEarlier, limit), nil
}
