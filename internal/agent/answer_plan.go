package agent

import (
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
	openai "github.com/sashabaranov/go-openai"
	"github.com/sqlc-dev/pqtype"

	"go-monolith/internal/answerplan"
	"go-monolith/internal/api"
	"go-monolith/internal/db"
	"go-monolith/metrics"
)

// The plan of an answer to a personal message (П55 phase 1): the code decides which of the
// person's memories a reply to "уморен съм" is woven from, and the model only writes it
// (internal/answerplan). This file joins it to the turn:
//
//   - prepareTurn asks the planner after the closing note of П49 is in place; a plan replaces
//     that note with its own (which ends the reply the same way: a question or a statement),
//     in the copy of the message the model reads - never in the stored one;
//   - the model marks a sentence that uses a memory with [1], [2]...; the marks are taken out
//     of the stream and of the stored answer, and counted;
//   - the stored answer keeps the plan in messages.answer_plan: which memories, which marks.
//
// Off unless ANSWER_PLAN_ENABLED (WithAnswerPlanner). Every failure on the way is a plain reply.

// recentPlansForMemories is how many of the person's last plans keep their memories from being
// used again while there are others.
const recentPlansForMemories = 3

// planLabelRunes is the length of the label of a memory in the stored plan (the line "Лили си
// спомни: ..." of the clients, step 2.5).
const planLabelRunes = 60

// WithAnswerPlanner turns on the plan of an answer.
func (o *Orchestrator) WithAnswerPlanner(p *answerplan.Planner) *Orchestrator {
	o.answerPlanner = p
	// Every label exists from the start, so that the panel shows the rates before the first plan.
	for _, r := range []string{answerplan.ResultPlanned, answerplan.SkipNotPersonal, answerplan.SkipNotDue,
		answerplan.SkipNoMemories, answerplan.SkipEmbedderError, answerplan.SkipCloud, answerplan.SkipTask,
		answerplan.SkipForcedTool, answerplan.SkipFile, answerplan.SkipAttachment, answerplan.SkipStatus,
		answerplan.SkipCode, answerplan.SkipLink, answerplan.SkipShort, answerplan.SkipGreeting} {
		metrics.AnswerPlans.WithLabelValues(planResultLabel(r))
	}
	metrics.AnswerPlanMarks.WithLabelValues("used")
	metrics.AnswerPlanMarks.WithLabelValues("unknown")
	return o
}

func planResultLabel(result string) string {
	if result == answerplan.ResultPlanned {
		return result
	}
	return "skipped_" + result
}

// planSignals are what the turn knows about its message besides the words.
func (o *Orchestrator) planSignals(ctx context.Context, message string, hasFiles, task bool) answerplan.Signals {
	usePaid, chosen := llmChoice(ctx)
	return answerplan.Signals{
		Cloud:          o.paid != nil && chosen && usePaid,
		Task:           task,
		ForcedTool:     forcedFirstTool(o.turnTools(), message) != "" || o.explainSubjectFor(message) != "",
		FileRequest:    asksToWrite(message) || asksToRemoveFile(message),
		Attachment:     hasFiles,
		StatusQuestion: asksForStatus(message),
	}
}

// withAnswerPlan puts the plan of this turn next to the last message of the history (the
// person's), in the place of the closing note. It returns the plan, nil when there is none.
func (o *Orchestrator) withAnswerPlan(ctx context.Context, userID uuid.UUID, sessionID string, c chatContext, s answerplan.Signals) (chatContext, *answerplan.Plan) {
	if o.answerPlanner == nil {
		return c, nil
	}
	last := len(c.History) - 1
	if last < 0 || c.History[last].Role != openai.ChatMessageRoleUser ||
		len(c.HistoryAt) != len(c.History) || c.HistoryAt[last].IsZero() {
		return c, nil
	}
	content := c.History[last].Content
	text := withoutClosingNote(content)

	turn := answerplan.Turn{
		Text:        text,
		Signals:     s,
		AskQuestion: strings.Contains(content, closingNoteQuestion),
	}
	// The cheap reasons first: most turns end here, without a query.
	if reason := answerplan.SkipReason(text, s); reason != "" {
		metrics.AnswerPlans.WithLabelValues(planResultLabel(reason)).Inc()
		return c, nil
	}
	planned, err := o.db.Queries.RecentAnswerPlanned(ctx, db.RecentAnswerPlannedParams{
		UserID:    userID,
		SessionID: sessionID,
		CreatedAt: sql.NullTime{Time: c.HistoryAt[last], Valid: true},
		Limit:     answerplan.Every - 1,
	})
	if err != nil {
		slog.WarnContext(ctx, "could not read the last answers for the plan; no plan", "err", err, "session_id", sessionID)
		metrics.AnswerPlans.WithLabelValues(planResultLabel(answerplan.SkipEmbedderError)).Inc()
		return c, nil
	}
	slices.Reverse(planned) // the newest first in the query, the oldest first for Due
	turn.RecentPlans = planned
	if !answerplan.Due(turn.RecentPlans) {
		metrics.AnswerPlans.WithLabelValues(planResultLabel(answerplan.SkipNotDue)).Inc()
		return c, nil
	}
	turn.Candidates = o.planCandidates(ctx, userID, c.MemoryHits)
	turn.UsedRecently = o.recentlyPlannedMemories(ctx, userID)

	plan, result := o.answerPlanner.Decide(ctx, turn)
	metrics.AnswerPlans.WithLabelValues(planResultLabel(result)).Inc()
	if plan == nil {
		return c, nil
	}
	ids := make([]string, len(plan.Memories))
	for i, m := range plan.Memories {
		ids[i] = m.ID.String()
	}
	slog.InfoContext(ctx, "answer plan", "session_id", sessionID, "memory_ids", ids,
		"score", plan.Score, "ask_question", plan.AskQuestion)
	c.History[last].Content = text + plan.Note()
	return c, plan
}

// planCandidates are the found memories with their vectors and fact keys; a failure is no candidates.
func (o *Orchestrator) planCandidates(ctx context.Context, userID uuid.UUID, ids []uuid.UUID) []answerplan.Memory {
	if len(ids) == 0 {
		return nil
	}
	rows, err := o.db.Queries.GetMemoryVectors(ctx, db.GetMemoryVectorsParams{UserID: userID, Ids: ids})
	if err != nil {
		slog.WarnContext(ctx, "could not read the vectors of the memories for the plan", "err", err)
		return nil
	}
	// In the order of the search.
	byID := make(map[uuid.UUID]db.GetMemoryVectorsRow, len(rows))
	for _, r := range rows {
		byID[r.ID] = r
	}
	out := make([]answerplan.Memory, 0, len(rows))
	for _, id := range ids {
		if r, ok := byID[id]; ok {
			out = append(out, answerplan.Memory{ID: r.ID, Content: r.Content, FactKey: r.FactKey, Vector: r.Embedding.Slice()})
		}
	}
	return out
}

// storedPlan is the plan of an answer as messages.answer_plan keeps it.
type storedPlan struct {
	Memories     []storedPlanMemory `json:"memories"`
	Mode         string             `json:"mode"`
	Marks        []int              `json:"marks"`
	UnknownMarks int                `json:"unknown_marks"`
	Score        float64            `json:"score"`
}

type storedPlanMemory struct {
	ID    uuid.UUID `json:"id"`
	Label string    `json:"label"`
}

// recentlyPlannedMemories are the memories of the person's last few plans.
func (o *Orchestrator) recentlyPlannedMemories(ctx context.Context, userID uuid.UUID) map[uuid.UUID]bool {
	rows, err := o.db.Queries.RecentAnswerPlans(ctx, db.RecentAnswerPlansParams{UserID: userID, Limit: recentPlansForMemories})
	if err != nil {
		slog.WarnContext(ctx, "could not read the last plans; every memory may be used", "err", err)
		return nil
	}
	used := map[uuid.UUID]bool{}
	for _, r := range rows {
		var p storedPlan
		if !r.Valid || json.Unmarshal(r.RawMessage, &p) != nil {
			continue
		}
		for _, m := range p.Memories {
			used[m.ID] = true
		}
	}
	return used
}

// planMark is a memory mark of a planned answer with the spaces before it: "на дивана [1]." is
// "на дивана.".
var planMark = regexp.MustCompile(`[ \t]*\[(\d{1,2})\]`)

// withoutPlanMarks is text without its memory marks, with the numbers it marked.
func withoutPlanMarks(text string) (string, []int) {
	var marks []int
	for _, m := range planMark.FindAllStringSubmatch(text, -1) {
		n, _ := strconv.Atoi(m[1])
		marks = append(marks, n)
	}
	return planMark.ReplaceAllString(text, ""), marks
}

func (t *turnState) setAnswerPlan(p *answerplan.Plan) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.plan = p
}

// answerPlan is the plan of the turn's answer; nil without one (and for a nil turn).
func (t *turnState) answerPlan() *answerplan.Plan {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.plan
}

// planReply is the answer of a turn as it is stored and sent on: without the memory marks
// when the turn had a plan, with the plan to store (not Valid without one). It counts the marks.
func (t *turnState) planReply(raw string) (string, pqtype.NullRawMessage) {
	plan := t.answerPlan()
	if plan == nil {
		return raw, pqtype.NullRawMessage{}
	}
	content, marks := withoutPlanMarks(raw)
	stored := storedPlan{Mode: "thought", Score: plan.Score, Marks: []int{}}
	for _, m := range plan.Memories {
		stored.Memories = append(stored.Memories, storedPlanMemory{ID: m.ID, Label: planLabel(m.Content)})
	}
	seen := map[int]bool{}
	for _, n := range marks {
		if n < 1 || n > len(plan.Memories) {
			stored.UnknownMarks++
			metrics.AnswerPlanMarks.WithLabelValues("unknown").Inc()
			continue
		}
		metrics.AnswerPlanMarks.WithLabelValues("used").Inc()
		if !seen[n] {
			seen[n] = true
			stored.Marks = append(stored.Marks, n)
		}
	}
	b, err := json.Marshal(stored)
	if err != nil { // only strings and numbers: not expected
		return content, pqtype.NullRawMessage{}
	}
	out := pqtype.NullRawMessage{RawMessage: b, Valid: true}
	t.mu.Lock()
	t.storedPlan = out
	t.mu.Unlock()
	return content, out
}

// remembered are the memories the stored answer used (api.RememberedMemories); nil before
// planReply and for a turn without a plan.
func (t *turnState) remembered() []string {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return api.RememberedMemories(t.storedPlan)
}

func (t *turnState) setPlanEvents(on bool) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.planEvents = on
}

// planFrame is the frame "event: plan" with the memories the answer used, for a client that asked
// for it (ChatRequest.PlanEvents); "" otherwise and when the answer used none.
func (t *turnState) planFrame() string {
	if t == nil {
		return ""
	}
	t.mu.Lock()
	wanted := t.planEvents
	t.mu.Unlock()
	remembered := t.remembered()
	if !wanted || len(remembered) == 0 {
		return ""
	}
	b, err := json.Marshal(map[string][]string{"remembered": remembered})
	if err != nil {
		return ""
	}
	return "event: plan\ndata: " + string(b) + "\n\n"
}

// withoutPlanMarksIfPlanned is the text without the marks when the turn had a plan, else as it is.
func (t *turnState) withoutPlanMarksIfPlanned(text string) string {
	if t.answerPlan() == nil {
		return text
	}
	out, _ := withoutPlanMarks(text)
	return out
}

// planLabel is the start of a memory, cut at a word.
func planLabel(content string) string {
	content = strings.Join(strings.Fields(content), " ")
	if utf8.RuneCountInString(content) <= planLabelRunes {
		return content
	}
	r := []rune(content)[:planLabelRunes]
	if i := strings.LastIndex(string(r), " "); i > 0 {
		return string(r)[:i] + "…"
	}
	return string(r) + "…"
}

// planMarkFilter takes the memory marks out of a streamed answer. A mark can be split between
// two pieces of the stream, so the end of a piece that may be the start of one ("на дивана [",
// even the spaces before it) waits for the next piece.
type planMarkFilter struct {
	buf string
}

var pendingPlanMark = regexp.MustCompile(`[ \t]*(\[\d{0,2})?$`)

func (f *planMarkFilter) Process(s string) string {
	f.buf = planMark.ReplaceAllString(f.buf+s, "")
	cut := len(f.buf)
	if loc := pendingPlanMark.FindStringIndex(f.buf); loc != nil {
		cut = loc[0]
	}
	out := f.buf[:cut]
	f.buf = f.buf[cut:]
	return out
}

func (f *planMarkFilter) Flush() string {
	out := planMark.ReplaceAllString(f.buf, "")
	f.buf = ""
	return out
}
