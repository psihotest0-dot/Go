// Package answerplan is the plan of a reply to a personal message (plan item П55, phase 1):
// the code decides what goes into the reply, the model only writes it.
//
// The owner's idea (2026-11-21): "уморен съм" should not get "почини си", but a reply woven
// from what Lili knows of him - the sofa, the television, something tasty. A prompt rule
// cannot make a 12B model do that (measured on 2026-11-21: true facts in the note, no use of
// them), so the code does the thinking: is the message personal (a vector check against
// examples), which of his memories fit it (nearness, corrected for the memories that are
// near everything), is it the turn for a plan at all (at most one reply in five), and how the
// reply ends (the dose of questions, П49). The model gets the memories by number in a note
// next to the message and writes 3-6 sentences from them and one thought of its own.
//
// Quotes from the books are out of phase 1 (the owner's decision, 2026-11-22): the sentence
// of a book nearest to a message is almost always a line of a novel, and of the philosophy
// books about 6 in 25 fit, with the same nearness as the ones that do not.
//
// The package has no model and no database: the caller gives it the memory candidates (with
// their vectors) and the signals of the turn; an embedder gives the vectors of the message.
package answerplan

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"github.com/google/uuid"

	"go-monolith/internal/llm"
	"go-monolith/internal/retrieval"
)

// personalThreshold is how much nearer the nearest personal example must be than the nearest
// example of the other kind (the "1-NN margin", both embedded as passages). Measured with
// harness H1 on the owner's own messages (2026-11-22, 93 labelled, 9 personal by his
// corrected labels): every personal one is at +0.033 or above; the outbursts of anger, which
// he wants to be listened to and not planned for, are at +0.029 and below. The threshold is in
// the middle. It rests on 9 messages - to be measured again once there are 30 more.
const personalThreshold = 0.031

// The choice of memories (measured on 2026-11-22 with the 30 invented memories and the 25
// invented personal messages; go-monolith-backups\measurements\p55_detector_2026-11-22,
// "Резултат 4"). The plain nearness of a memory does not tell a fit: a few memories are at
// 0.80-0.84 to almost any message. So a memory's nearness to the message is lowered by its
// mean nearness to the not-personal examples (its "nearness to everything"); what is left is
// +0.06-0.10 for a fitting memory ("диван" for the evening on the sofa) and +0.02-0.05 for the
// rest. A memory is used when it is at least memoryMinLift above its baseline and within
// memoryNearBest of the best one - so one clear fit does not bring two weak ones with it.
//
// memoryMinLift was 0.05 until the owner's blind test of 2026-11-22 (step 2.4, measurements\
// p55_blind_2026-11-22): the planned answer won 7 of 15 against today's, 5 of 8 where its best
// memory was at 0.07 or more and 2 of 7 below - a weak memory ("слуша рок, когато шофира" for
// "ще си почивам") makes the reply worse, and today's reply weaves the memories of the prompt
// anyway. 0.07 rests on those 15 pairs; the second round measures it on new messages.
const (
	memoryMinLift  = 0.07
	memoryNearBest = 0.015
	maxMemories    = 3
	// minFreshMemories: with at least this many fitting memories that the last plans did not
	// use, the used ones stay out; with fewer, they may come back.
	minFreshMemories = 2
)

// Every is the distance of the plans in a conversation: at most one reply in Every has one,
// so never two in a row. A plan that comes too often sounds like a template.
const Every = 5

// Results of a turn, the labels of the metric answer_plan_total besides "skipped_<reason>".
const (
	ResultPlanned     = "planned"
	SkipNotDue        = "not_due"      // a plan was among the last Every-1 replies
	SkipNotPersonal   = "not_personal" // the detector says no
	SkipNoMemories    = "no_memories"  // personal, but no memory fits it
	SkipEmbedderError = "error"        // the embedder failed: a plain reply
)

// Memory is a memory the plan may use. Vector is its stored embedding (a passage);
// FactKey the key of a fact ("name", ...) when it is one, so that two values of one fact are
// never used together.
type Memory struct {
	ID      uuid.UUID
	Content string
	FactKey string
	Vector  []float32
}

// Turn is what the caller knows of the turn.
type Turn struct {
	Text    string
	Signals Signals
	// RecentPlans are the earlier replies of the conversation, the oldest first: true for a
	// reply that had a plan.
	RecentPlans []bool
	// Candidates are the memories the search found for the message, in any order.
	Candidates []Memory
	// UsedRecently are the memories of the person's last few plans; they are taken only
	// when there are not enough others (while the memories are few, a repeat is allowed).
	UsedRecently map[uuid.UUID]bool
	// AskQuestion is the dose of questions for this reply (closingNoteFor): end it with one
	// question, or with a statement.
	AskQuestion bool
}

// Plan is the plan of a reply.
type Plan struct {
	Memories    []Memory
	AskQuestion bool
	// Score is the detector's margin, for the log.
	Score float64
}

// Planner holds the vectors of the examples; it is made once, at the start.
type Planner struct {
	embedder llm.Embedder
	// personal and notPersonal are the examples embedded as passages (the detector);
	// neutral are the not-personal examples embedded as queries, as a message is for the
	// memory search (the baseline of a memory).
	personal, notPersonal, neutral [][]float32
}

// NewPlanner embeds the examples (75 short texts, cached by the embedder after the first start).
func NewPlanner(ctx context.Context, embedder llm.Embedder) (*Planner, error) {
	examples := slices.Concat(personalExamples, notPersonalExamples)
	passages, err := embedder.EmbedTexts(ctx, examples, false)
	if err != nil {
		return nil, fmt.Errorf("embed the examples of the answer plan: %w", err)
	}
	neutral, err := embedder.EmbedTexts(ctx, notPersonalExamples, true)
	if err != nil {
		return nil, fmt.Errorf("embed the neutral examples of the answer plan: %w", err)
	}
	if len(passages) != len(examples) || len(neutral) != len(notPersonalExamples) {
		return nil, fmt.Errorf("embed the examples of the answer plan: %d and %d vectors for %d and %d texts",
			len(passages), len(neutral), len(examples), len(notPersonalExamples))
	}
	return &Planner{
		embedder:    embedder,
		personal:    passages[:len(personalExamples)],
		notPersonal: passages[len(personalExamples):],
		neutral:     neutral,
	}, nil
}

// Due tells whether a reply after these replies (the oldest first, true = had a plan) may
// have one: none of the last Every-1 had.
func Due(recentPlans []bool) bool {
	from := max(0, len(recentPlans)-(Every-1))
	return !slices.Contains(recentPlans[from:], true)
}

// Decide is the plan of the turn, or the reason there is none (one of the Skip... values,
// without the "skipped_" prefix). The cheap checks go first, so most turns cost nothing.
func (p *Planner) Decide(ctx context.Context, t Turn) (*Plan, string) {
	if reason := SkipReason(t.Text, t.Signals); reason != "" {
		return nil, reason
	}
	if !Due(t.RecentPlans) {
		return nil, SkipNotDue
	}
	vectors, err := p.embedder.EmbedTexts(ctx, []string{t.Text}, false)
	if err != nil || len(vectors) != 1 {
		slog.WarnContext(ctx, "answer plan: the message could not be embedded; a plain reply", "err", err, "vectors", len(vectors))
		return nil, SkipEmbedderError
	}
	score := p.margin(vectors[0])
	if score < personalThreshold {
		return nil, SkipNotPersonal
	}
	query, err := p.embedder.EmbedTexts(ctx, []string{t.Text}, true)
	if err != nil || len(query) != 1 {
		slog.WarnContext(ctx, "answer plan: the query could not be embedded; a plain reply", "err", err, "vectors", len(query))
		return nil, SkipEmbedderError
	}
	memories := p.pickMemories(query[0], t.Candidates, t.UsedRecently)
	if len(memories) == 0 {
		return nil, SkipNoMemories
	}
	return &Plan{Memories: memories, AskQuestion: t.AskQuestion, Score: score}, ResultPlanned
}

// margin is the detector's score of a message (embedded as a passage): the nearness of the
// nearest personal example minus that of the nearest not-personal one.
func (p *Planner) margin(v []float32) float64 {
	return nearest(v, p.personal) - nearest(v, p.notPersonal)
}

func nearest(v []float32, examples [][]float32) float64 {
	best := -1.0
	for _, e := range examples {
		best = max(best, float64(retrieval.CosineSimilarity(v, e)))
	}
	return best
}

// lift is a memory's nearness to the message (embedded as a query) above its mean nearness
// to the neutral messages.
func (p *Planner) lift(query, memory []float32) float64 {
	base := 0.0
	for _, n := range p.neutral {
		base += float64(retrieval.CosineSimilarity(n, memory))
	}
	return float64(retrieval.CosineSimilarity(query, memory)) - base/float64(len(p.neutral))
}

// pickMemories chooses up to maxMemories memories for the message: the ones that fit it
// (memoryMinLift, memoryNearBest), the best first, one per fact; the recently used ones only
// when fewer than minFreshMemories others fit.
func (p *Planner) pickMemories(query []float32, candidates []Memory, usedRecently map[uuid.UUID]bool) []Memory {
	type scored struct {
		m    Memory
		lift float64
	}
	var fits []scored
	seen := map[uuid.UUID]bool{}
	for _, m := range candidates {
		if len(m.Vector) == 0 || seen[m.ID] || strings.TrimSpace(m.Content) == "" {
			continue
		}
		seen[m.ID] = true
		if l := p.lift(query, m.Vector); l >= memoryMinLift {
			fits = append(fits, scored{m, l})
		}
	}
	if len(fits) == 0 {
		return nil
	}
	slices.SortStableFunc(fits, func(a, b scored) int {
		switch {
		case a.lift > b.lift:
			return -1
		case a.lift < b.lift:
			return 1
		}
		return 0
	})
	best := fits[0].lift
	var fresh, used []Memory
	for _, f := range fits {
		if f.lift < best-memoryNearBest {
			break
		}
		if usedRecently[f.m.ID] {
			used = append(used, f.m)
		} else {
			fresh = append(fresh, f.m)
		}
	}
	pool := fresh
	if len(fresh) < minFreshMemories {
		pool = slices.Concat(fresh, used)
	}
	var out []Memory
	keys := map[string]bool{}
	for _, m := range pool {
		if len(out) == maxMemories {
			break
		}
		if m.FactKey != "" {
			if keys[m.FactKey] {
				continue
			}
			keys[m.FactKey] = true
		}
		out = append(out, m)
	}
	return out
}
