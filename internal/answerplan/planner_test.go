package answerplan

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The fake embedder puts texts on axes: 0 - personal, 1 - not personal (and neutral), 2-4 -
// the topics of the memories. The examples sit on their axes, so a message's margin and a
// memory's lift are exact numbers.
const dims = 5

func axis(i int) []float32 {
	v := make([]float32, dims)
	v[i] = 1
	return v
}

func vec(parts map[int]float64) []float32 {
	v := make([]float32, dims)
	var n float64
	for _, x := range parts {
		n += x * x
	}
	for i, x := range parts {
		v[i] = float32(x / math.Sqrt(n))
	}
	return v
}

type fakeEmbedder struct {
	passages, queries map[string][]float32
	err               error
	calls             int
}

func newFake() *fakeEmbedder {
	f := &fakeEmbedder{passages: map[string][]float32{}, queries: map[string][]float32{}}
	for _, s := range personalExamples {
		f.passages[s] = axis(0)
	}
	for _, s := range notPersonalExamples {
		f.passages[s] = axis(1)
		f.queries[s] = axis(1)
	}
	return f
}

func (f *fakeEmbedder) EmbedTexts(_ context.Context, texts []string, isQuery bool) ([][]float32, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	table := f.passages
	if isQuery {
		table = f.queries
	}
	out := make([][]float32, len(texts))
	for i, t := range texts {
		v, ok := table[t]
		if !ok {
			return nil, errors.New("no vector for " + t)
		}
		out[i] = v
	}
	return out, nil
}

// withMargin is a passage vector whose margin (personal minus not) is exactly m.
func withMargin(m float64) []float32 {
	theta := math.Acos(m/math.Sqrt2) - math.Pi/4
	return vec(map[int]float64{0: math.Cos(theta), 1: math.Sin(theta)})
}

func newPlanner(t *testing.T, f *fakeEmbedder) *Planner {
	t.Helper()
	p, err := NewPlanner(context.Background(), f)
	require.NoError(t, err)
	return p
}

const tired = "уморен съм, тежка седмица"

// personalFake knows a personal message whose query vector points at topic 2.
func personalFake() *fakeEmbedder {
	f := newFake()
	f.passages[tired] = withMargin(0.2)
	f.queries[tired] = axis(2)
	return f
}

func mem(content string, v []float32) Memory {
	return Memory{ID: uuid.New(), Content: content, Vector: v}
}

func TestDetectorThreshold(t *testing.T) {
	for _, tc := range []struct {
		margin float64
		want   string
	}{
		{personalThreshold + 0.002, ResultPlanned},
		{personalThreshold - 0.002, SkipNotPersonal},
		{-0.1, SkipNotPersonal},
	} {
		f := personalFake()
		f.passages[tired] = withMargin(tc.margin)
		p := newPlanner(t, f)
		plan, got := p.Decide(context.Background(), Turn{Text: tired, Candidates: []Memory{mem("Диванът.", axis(2))}})
		assert.Equal(t, tc.want, got, "margin %.3f", tc.margin)
		if tc.want == ResultPlanned {
			require.NotNil(t, plan)
			assert.InDelta(t, tc.margin, plan.Score, 1e-4)
		} else {
			assert.Nil(t, plan)
		}
	}
}

func TestMemoriesNearEverythingLoseToOnesThatFit(t *testing.T) {
	p := newPlanner(t, personalFake())
	// The hub is nearer to the message than the fitting one (0.958 against 0.894), but it is
	// as near to the neutral messages (0.287): its lift is 0.67, the fitting one's 0.894.
	hub := mem("Дразни го, когато нещата се обещават.", vec(map[int]float64{1: 0.3, 2: 1}))
	fit := mem("Почива на дивана пред телевизора.", vec(map[int]float64{2: 1, 3: 0.5}))
	plan, result := p.Decide(context.Background(), Turn{Text: tired, Candidates: []Memory{hub, fit}})
	require.Equal(t, ResultPlanned, result)
	assert.Equal(t, []Memory{fit}, plan.Memories)
}

func TestMemoriesAreTheBestFewOnePerFact(t *testing.T) {
	p := newPlanner(t, personalFake())
	best := mem("A", axis(2))
	close1 := mem("B", vec(map[int]float64{2: 1, 3: 0.1}))  // 0.995
	close2 := mem("C", vec(map[int]float64{2: 1, 4: 0.11})) // 0.994
	close3 := mem("D", vec(map[int]float64{2: 1, 3: 0.12})) // 0.993
	far := mem("E", vec(map[int]float64{2: 1, 3: 0.3}))     // 0.958: further than best-0.015
	none := mem("F", axis(3))                               // 0: under memoryMinLift
	noVector := Memory{ID: uuid.New(), Content: "G"}
	empty := mem("  ", axis(2))

	plan, _ := p.Decide(context.Background(), Turn{Text: tired,
		Candidates: []Memory{none, far, close3, close2, close1, best, noVector, empty, best}})
	require.NotNil(t, plan)
	assert.Equal(t, []string{"A", "B", "C"}, contents(plan.Memories), "the three nearest, the best first, each once")

	// Two values of one fact: the nearer one only.
	k1, k2 := best, close1
	k1.FactKey, k2.FactKey = "name", "name"
	plan, _ = p.Decide(context.Background(), Turn{Text: tired, Candidates: []Memory{k2, k1, close2}})
	require.NotNil(t, plan)
	assert.Equal(t, []string{"A", "C"}, contents(plan.Memories))
}

func TestRecentlyUsedMemoriesOnlyWhenTooFewOthers(t *testing.T) {
	p := newPlanner(t, personalFake())
	a := mem("A", axis(2))
	b := mem("B", vec(map[int]float64{2: 1, 3: 0.1}))
	c := mem("C", vec(map[int]float64{2: 1, 4: 0.1}))

	plan, _ := p.Decide(context.Background(), Turn{Text: tired, Candidates: []Memory{a, b, c},
		UsedRecently: map[uuid.UUID]bool{a.ID: true}})
	require.NotNil(t, plan)
	assert.Equal(t, []string{"B", "C"}, contents(plan.Memories), "two fresh ones: the used one stays out")

	plan, _ = p.Decide(context.Background(), Turn{Text: tired, Candidates: []Memory{a, b},
		UsedRecently: map[uuid.UUID]bool{a.ID: true}})
	require.NotNil(t, plan)
	assert.Equal(t, []string{"B", "A"}, contents(plan.Memories), "one fresh one: the used one comes after it")

	plan, result := p.Decide(context.Background(), Turn{Text: tired, Candidates: []Memory{a},
		UsedRecently: map[uuid.UUID]bool{a.ID: true}})
	require.Equal(t, ResultPlanned, result)
	assert.Equal(t, []string{"A"}, contents(plan.Memories), "only a used one: it is used again")
}

func TestNoFittingMemoryNoPlan(t *testing.T) {
	p := newPlanner(t, personalFake())
	plan, result := p.Decide(context.Background(), Turn{Text: tired, Candidates: []Memory{mem("F", axis(3))}})
	assert.Nil(t, plan)
	assert.Equal(t, SkipNoMemories, result)
	_, result = p.Decide(context.Background(), Turn{Text: tired})
	assert.Equal(t, SkipNoMemories, result)
}

func TestCheapChecksComeBeforeTheEmbedder(t *testing.T) {
	f := personalFake()
	p := newPlanner(t, f)
	f.calls = 0
	_, result := p.Decide(context.Background(), Turn{Text: tired, RecentPlans: []bool{true, false}})
	assert.Equal(t, SkipNotDue, result)
	_, result = p.Decide(context.Background(), Turn{Text: tired, Signals: Signals{Cloud: true}})
	assert.Equal(t, SkipCloud, result)
	assert.Zero(t, f.calls, "a skipped turn embeds nothing")
}

func TestEmbedderFailureIsAPlainReply(t *testing.T) {
	f := personalFake()
	p := newPlanner(t, f)
	f.err = errors.New("down")
	plan, result := p.Decide(context.Background(), Turn{Text: tired, Candidates: []Memory{mem("A", axis(2))}})
	assert.Nil(t, plan)
	assert.Equal(t, SkipEmbedderError, result)

	// The passage is known, the query vector is not.
	f = personalFake()
	p = newPlanner(t, f)
	delete(f.queries, tired)
	_, result = p.Decide(context.Background(), Turn{Text: tired, Candidates: []Memory{mem("A", axis(2))}})
	assert.Equal(t, SkipEmbedderError, result)
}

func TestNewPlannerFails(t *testing.T) {
	f := newFake()
	f.err = errors.New("down")
	_, err := NewPlanner(context.Background(), f)
	assert.Error(t, err)

	_, err = NewPlanner(context.Background(), shortEmbedder{})
	assert.ErrorContains(t, err, "vectors for")
}

type shortEmbedder struct{}

func (shortEmbedder) EmbedTexts(_ context.Context, texts []string, _ bool) ([][]float32, error) {
	return make([][]float32, len(texts)-1), nil
}

func TestDue(t *testing.T) {
	assert.True(t, Due(nil))
	assert.True(t, Due([]bool{false, false}))
	assert.False(t, Due([]bool{true}), "never two in a row")
	assert.False(t, Due([]bool{true, false, false, false}), "a plan 4 replies ago is too near")
	assert.True(t, Due([]bool{true, false, false, false, false}), "one in five")
	assert.True(t, Due([]bool{true, true, false, false, false, false}))
}

func TestPlanAsksTheEndingOfTheDose(t *testing.T) {
	plan := &Plan{Memories: []Memory{
		{Content: "Ивайло си почива на дивана пред телевизора."},
		{Content: "Ивайло обича баница\n и   шопска салата."},
		{Content: strings.Repeat("дълго ", 100)},
	}}
	note := plan.Note()
	assert.Contains(t, note, "[1] Ивайло си почива на дивана пред телевизора.\n")
	assert.Contains(t, note, "[2] Ивайло обича баница и шопска салата.\n", "one line per memory")
	assert.Contains(t, note, "[3] дълго")
	assert.Less(t, strings.Index(note, "[3]"), strings.Index(note, noteRules))
	assert.Contains(t, note, "…\n", "a long memory is cut")
	assert.True(t, strings.HasSuffix(note, noteEndStatement))
	assert.Contains(t, note, "not a quote, no book, no author")

	plan.AskQuestion = true
	assert.True(t, strings.HasSuffix(plan.Note(), noteEndQuestion))
}

func TestWithoutNote(t *testing.T) {
	plan := &Plan{Memories: []Memory{{Content: "A"}}}
	msg := "уморен съм"
	assert.Equal(t, msg, WithoutNote(msg+plan.Note()))
	assert.Equal(t, msg+" [other note]", WithoutNote(msg+plan.Note()+" [other note]"))
	plan.AskQuestion = true
	assert.Equal(t, msg, WithoutNote(msg+plan.Note()))
	assert.Equal(t, msg, WithoutNote(msg))
	broken := msg + notePrefix + "[1] A"
	assert.Equal(t, broken, WithoutNote(broken), "a note without its end is left as it is")
}

func contents(ms []Memory) []string {
	out := make([]string, len(ms))
	for i, m := range ms {
		out[i] = m.Content
	}
	return out
}
