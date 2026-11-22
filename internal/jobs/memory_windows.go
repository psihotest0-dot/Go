package jobs

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	openai "github.com/sashabaranov/go-openai"

	"go-monolith/internal/db"
	"go-monolith/internal/prompts"
)

// The memory analysis reads a conversation, not each message alone (plan step 1.2, M1 + M3 from the
// review of AnythingLLM, 2026-11-22): a fact often needs the words around it ("Ивайло предпочита да
// почива пред телевизора с вкусна храна" came from three messages and a question of Lili). A
// conversation is read when it has been quiet for windowQuiet; one with at least
// windowMinPersonMessages new messages of the person is read in windows of the person's and Lili's
// lines, a shorter one message by message as before.
//
// What the model says is checked by the code, as for a single message (fact_evidence.go): the
// evidence must be in the PERSON's lines of the window (in the night measurement of 2026-11-22 a
// fact came from Lili's words: "притежава търговска система" from "За теб като човек с търговска
// система"), at most windowMaxFacts facts a window. The memories already known are listed in the
// prompt so that the model does not return them again; it decides nothing about them. A second call
// that chose "update memory #n" replaced the memory with the name in the same measurement, and a
// limit on the distance of the vectors cannot tell a repeat from a change: with this embedder
// "живее в София" / "живее във Варна" are 0.028 apart and "е на 45" / "е на 46 години" 0.017, nearer
// than the same fact in other words (0.013-0.14; measurements\memdedupe_2026-11-22). Repeats and
// changes are left to the content hash and to decideFact, as for a single message.

const (
	// windowQuiet: a conversation is read when its last message is this old, and a pause this long
	// ends a window.
	windowQuiet = 20 * time.Minute
	// windowMinPersonMessages: fewer new messages of the person are read one by one.
	windowMinPersonMessages = 5
	// windowMaxMessages and windowMaxRunes bound a window and each message in it.
	windowMaxMessages = 20
	windowMaxRunes    = 1500
	// windowMaxFacts is the most facts kept from one window.
	windowMaxFacts = 3
	// windowKnownMemories is how many known memories (the most important) the prompt lists.
	windowKnownMemories = 30
	// windowAssistant is how Lili's lines are labelled.
	windowAssistant = "Лили"
)

// analysisPlan is how the unread messages of a user are read in one run.
type analysisPlan struct {
	// windows are conversations read as a whole, each window in the order of its messages.
	windows [][]db.Message
	// single are read one by one, as before.
	single []db.Message
	// waiting is how many messages wait for their conversation to become quiet.
	waiting int
}

// planAnalysis sorts the unread messages (the oldest first) by conversation.
func planAnalysis(msgs []db.Message, now time.Time) analysisPlan {
	var plan analysisPlan
	bySession := map[string][]db.Message{}
	var order []string
	for _, m := range msgs {
		if _, ok := bySession[m.SessionID]; !ok {
			order = append(order, m.SessionID)
		}
		bySession[m.SessionID] = append(bySession[m.SessionID], m)
	}
	for _, s := range order {
		group := bySession[s]
		if newest := group[len(group)-1].CreatedAt; newest.Valid && now.Sub(newest.Time) < windowQuiet {
			plan.waiting += len(group)
			continue
		}
		person, retried := 0, false
		for _, m := range group {
			if m.Role == "user" && strings.TrimSpace(m.Content) != "" {
				person++
				// A window that failed before is not tried again as a window: its messages go one
				// by one, which counts and ends their attempts as it always did.
				retried = retried || m.AnalysisAttempts > 0
			}
		}
		if person < windowMinPersonMessages || retried {
			plan.single = append(plan.single, group...)
			continue
		}
		var cur []db.Message
		flush := func() {
			if len(cur) == 0 {
				return
			}
			if personLines(cur) == 0 {
				plan.single = append(plan.single, cur...)
			} else {
				plan.windows = append(plan.windows, cur)
			}
			cur = nil
		}
		for i, m := range group {
			if i > 0 && (len(cur) >= windowMaxMessages || (m.CreatedAt.Valid && group[i-1].CreatedAt.Valid && m.CreatedAt.Time.Sub(group[i-1].CreatedAt.Time) >= windowQuiet)) {
				flush()
			}
			cur = append(cur, m)
		}
		flush()
	}
	return plan
}

// personLines is how many messages of the person a window has.
func personLines(w []db.Message) int {
	n := 0
	for _, m := range w {
		if m.Role == "user" && strings.TrimSpace(m.Content) != "" {
			n++
		}
	}
	return n
}

// windowLine is one message as the window shows it: on one line, cut to windowMaxRunes.
func windowLine(content string) string {
	s := strings.Join(strings.Fields(content), " ")
	if utf8.RuneCountInString(s) > windowMaxRunes {
		s = string([]rune(s)[:windowMaxRunes]) + "…"
	}
	return s
}

// windowText is the conversation as the model reads it, and the person's lines alone (what the
// evidence of a fact must be in).
func windowText(w []db.Message, person string) (conversation, personText string) {
	var all, own strings.Builder
	for _, m := range w {
		line := windowLine(m.Content)
		if line == "" {
			continue
		}
		who := windowAssistant
		if m.Role == "user" {
			who = person
			own.WriteString(line + "\n")
		}
		fmt.Fprintf(&all, "%s: %s\n", who, line)
	}
	return all.String(), own.String()
}

// sourceOf is the message of the person a fact rests on: the one its evidence is in, else the last
// one of the person in the window.
func sourceOf(w []db.Message, fact Fact) db.Message {
	evidence := trimEvidence(normalizeForEvidence(fact.Evidence))
	var last db.Message
	for _, m := range w {
		if m.Role != "user" {
			continue
		}
		last = m
		if evidence != "" && strings.Contains(normalizeForEvidence(windowLine(m.Content)), evidence) {
			return m
		}
	}
	return last
}

// knownMemories are the memories in force the window prompt lists, the most important first.
func (w *MemoryConsolidationWorker) knownMemories(ctx context.Context, userID uuid.UUID) []string {
	top, err := w.dbClient.Queries.GetTopMemories(ctx, db.GetTopMemoriesParams{UserID: userID, Limit: windowKnownMemories})
	if err != nil {
		slog.Warn("could not read the known memories for the window prompt; it is written without them", "error", err)
		return nil
	}
	known := make([]string, 0, len(top))
	for _, m := range top {
		known = append(known, windowLine(m.Content))
	}
	return known
}

// analyzeWindow asks the model for the facts of a window and keeps those that stand on the person's
// words, at most windowMaxFacts.
func (w *MemoryConsolidationWorker) analyzeWindow(ctx context.Context, userID uuid.UUID, window []db.Message) ([]Fact, error) {
	name := w.knownName(ctx, userID)
	person := name
	if person == "" {
		person = "Потребителят"
	}
	conversation, own := windowText(window, person)
	systemPrompt, err := prompts.GlobalManager.Render("memory_window.tmpl", map[string]any{
		"Name": name, "Person": person, "Known": w.knownMemories(ctx, userID),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to render the window prompt: %w", err)
	}
	req := openai.ChatCompletionRequest{
		Messages: []openai.ChatCompletionMessage{
			{Role: openai.ChatMessageRoleSystem, Content: systemPrompt},
			{Role: openai.ChatMessageRoleUser, Content: conversation},
		},
		//lint:ignore SA1019 llama-server and DeepSeek read max_tokens; max_completion_tokens is OpenAI's own name.
		MaxTokens:   700,
		Temperature: 0.1,
	}
	resp, err := w.llmClient.ChatWithJSONSchema(ctx, req, []Fact{})
	if err != nil {
		return nil, err
	}
	facts, err := parseFacts(resp)
	if err != nil {
		return nil, err
	}
	facts = groundedFacts(own, facts)
	if len(facts) > windowMaxFacts {
		facts = facts[:windowMaxFacts]
	}
	return facts, nil
}

// readWindow analyses one window and stores its facts; the messages of the window are marked read
// when it worked, and the person's get a failed attempt when it did not (the next run then reads
// them one by one, see planAnalysis).
func (w *MemoryConsolidationWorker) readWindow(ctx context.Context, userID uuid.UUID, window []db.Message) {
	analyzeCtx, cancel := context.WithTimeout(ctx, 180*time.Second)
	facts, err := w.analyzeWindow(analyzeCtx, userID, window)
	cancel()
	failed := err != nil
	if err != nil {
		slog.Error("failed to analyze a window of the conversation", "messages", len(window), "error", err)
	}
	for _, fact := range facts {
		processCtx, processCancel := context.WithTimeout(ctx, 30*time.Second)
		if err := w.processFact(processCtx, sourceOf(window, fact), fact); err != nil {
			slog.Error("failed to process fact", "error", err)
			failed = true
		}
		processCancel()
	}
	for _, m := range window {
		switch {
		case !failed:
			w.markAnalyzed(ctx, m.ID, m.UserID)
		case m.Role == "user":
			w.countFailedAnalysis(ctx, m.ID)
		}
	}
}
