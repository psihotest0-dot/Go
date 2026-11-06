package oracle

import (
	"context"
	"regexp"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var versionShape = regexp.MustCompile(`^[0-9a-f]{12}$`)

func TestThePromptVersionIsTwelveHexCharactersAndStable(t *testing.T) {
	a := currentPromptParts("SYSTEM PROMPT").version()
	b := currentPromptParts("SYSTEM PROMPT").version()

	assert.Regexp(t, versionShape, a)
	assert.Equal(t, a, b, "the same question has the same label, call after call")
}

// Each part is something the owner can change, and each change must start a new period.
func TestEveryPartOfTheQuestionMovesTheVersion(t *testing.T) {
	base := currentPromptParts("SYSTEM PROMPT")
	want := base.version()

	changes := map[string]func(p *promptParts){
		"the system prompt": func(p *promptParts) { p.System += " (edited)" },
		"the answer schema": func(p *promptParts) { p.Schema += " " },
		"the call settings": func(p *promptParts) { p.Params = "temperature=0.9 max_tokens=200 attempts=2 reason_runes=200" },
		"the facts layout":  func(p *promptParts) { p.UserSample += "\n" },
	}
	seen := map[string]string{want: "the base"}
	for name, change := range changes {
		p := base
		change(&p)
		got := p.version()
		assert.NotEqual(t, want, got, "a change to %s must change the label", name)
		if other, dup := seen[got]; dup {
			t.Errorf("a change to %s gave the same label as %s", name, other)
		}
		seen[got] = name
	}
}

// Without the name and the length in front of each part, text could slide from one
// part into the next and the label would not notice.
func TestTextCannotMoveBetweenPartsUnnoticed(t *testing.T) {
	a := promptParts{System: "ab", Schema: "c"}
	b := promptParts{System: "a", Schema: "bc"}

	assert.NotEqual(t, a.version(), b.version())
}

// What the label stands on must really be what the Service sends.
func TestTheCurrentPartsAreWhatTheServiceSends(t *testing.T) {
	p := currentPromptParts("SYSTEM PROMPT")

	assert.Equal(t, "SYSTEM PROMPT", p.System)
	assert.Contains(t, p.Schema, "APPROVE", "the enum of the verdict is in the schema")
	assert.Contains(t, p.Schema, "HOLD")
	assert.Contains(t, p.Params, "temperature=0.2")
	assert.Contains(t, p.Params, "max_tokens=200")
	assert.Contains(t, p.UserSample, "Signal\n", "the sample is laid out like a real question")
	assert.Contains(t, p.UserSample, "quoted text from outside")
	assert.Contains(t, p.UserSample, "Live price: ")
}

func TestTheSampleQuestionIsLaidOutTheWayTheModelSeesIt(t *testing.T) {
	r, _ := mustDecode(t)

	assert.Contains(t, r.userMessage(), "symbol: AAPL", "the real layout")
	assert.Contains(t, sampleUserMessage(), "symbol: SAMPLE", "the sample uses the same function")
}

func TestTheServiceStampsTheAnswerTheJournalAndTheRepeat(t *testing.T) {
	model := &fakeLLM{replies: []reply{good(VerdictVeto, "Лоша новина.")}}
	svc, store, _ := newTestService(model)
	req, raw := mustDecode(t)
	want := currentPromptParts("SYSTEM PROMPT").version()

	res, err := svc.Evaluate(context.Background(), req, raw, "r1")
	require.NoError(t, err)
	assert.Equal(t, want, res.PromptVersion, "the reply to the bot")
	rows := store.snapshot()
	require.Len(t, rows, 1)
	assert.Equal(t, want, rows[0].PromptVersion, "the journal row")

	// The prompt is edited; the repeat of the verdict must still carry the label of the
	// call that gave it, not of the prompt now in force.
	svc.prompt = func() (string, error) { return "A DIFFERENT PROMPT", nil }
	again, err := svc.Evaluate(context.Background(), req, raw, "r2")
	require.NoError(t, err)
	require.True(t, again.Cached)
	assert.Equal(t, want, again.PromptVersion)
	assert.Equal(t, 1, model.callCount(), "no second call to the model")
}

func TestAFailedCallIsStampedWithTheQuestionItWouldHaveAsked(t *testing.T) {
	model := &fakeLLM{replies: []reply{{err: assert.AnError}}}
	svc, store, _ := newTestService(model)
	req, raw := mustDecode(t)

	_, err := svc.Evaluate(context.Background(), req, raw, "r1")
	require.Error(t, err)

	rows := store.snapshot()
	require.Len(t, rows, 1)
	require.Equal(t, StatusFailed, rows[0].Status)
	assert.Equal(t, currentPromptParts("SYSTEM PROMPT").version(), rows[0].PromptVersion)
}

func TestADifferentPromptGivesTheNextAnswerAnotherVersion(t *testing.T) {
	model := &fakeLLM{replies: []reply{good(VerdictApprove, "a"), good(VerdictApprove, "b")}}
	svc, store, _ := newTestService(model)
	first, rawFirst := mustDecode(t)
	second, rawSecond := mustDecode(t, func(b map[string]any) {
		b["signal"].(map[string]any)["signal_id"] = "sig-0002"
	})

	_, err := svc.Evaluate(context.Background(), first, rawFirst, "r1")
	require.NoError(t, err)
	svc.prompt = func() (string, error) { return "SYSTEM PROMPT, EDITED", nil }
	_, err = svc.Evaluate(context.Background(), second, rawSecond, "r2")
	require.NoError(t, err)

	rows := store.snapshot()
	require.Len(t, rows, 2)
	assert.NotEqual(t, rows[0].PromptVersion, rows[1].PromptVersion, "a new period")
	assert.Regexp(t, versionShape, rows[0].PromptVersion)
	assert.Regexp(t, versionShape, rows[1].PromptVersion)
}
