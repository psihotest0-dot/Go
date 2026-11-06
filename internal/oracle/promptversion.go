package oracle

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"sync"

	"github.com/sashabaranov/go-openai/jsonschema"
)

// promptVersionLen is the length of the label in hex characters: 48 bits, which is
// plenty to tell the handful of prompts one deployment goes through, and short enough
// to read in a table.
const promptVersionLen = 12

// promptParts is everything on this side that decides WHAT the model is asked and how
// its answer is held. The label is a hash of all four, so a change to any of them
// gives the next answers a new label. The bot's evaluation (docs/oracle-provider-task.md
// in its repository, section 5) says that a changed prompt starts a new period; this is
// how a period is told apart afterwards instead of being guessed from dates.
//
// What is NOT in it: the model (kept in its own column), the model server's settings
// (context size, its default sampling), and anything the LLM client adds around the
// request. Those live outside this package.
type promptParts struct {
	System     string // the system prompt as rendered (so a PROMPTS_DIR override counts)
	Schema     string // the JSON schema the reply is held to, as sent
	Params     string // temperature, token limit, attempts and the limit on the reason
	UserSample string // the facts of a fixed sample request, as they are laid out for the model
}

// currentPromptParts is what the Service sends now, given the system prompt it rendered.
func currentPromptParts(system string) promptParts {
	return promptParts{
		System: system,
		Schema: answerSchemaJSON(),
		Params: fmt.Sprintf("temperature=%v max_tokens=%d attempts=%d reason_runes=%d",
			temperature, maxAnswerTokens, maxAttempts, maxReasonRunes),
		UserSample: sampleUserMessage(),
	}
}

// version is the label: the first promptVersionLen hex characters of a SHA-256 over the
// four parts. Each part is written with its name and length, so text cannot move from
// one part to the next without the label changing.
func (p promptParts) version() string {
	h := sha256.New()
	for _, part := range []struct{ name, value string }{
		{"system", p.System},
		{"schema", p.Schema},
		{"params", p.Params},
		{"user", p.UserSample},
	} {
		fmt.Fprintf(h, "%s:%d:", part.name, len(part.value))
		_, _ = io.WriteString(h, part.value)
	}
	return hex.EncodeToString(h.Sum(nil))[:promptVersionLen]
}

// answerSchemaJSON is the schema of Answer exactly as the LLM client generates it
// for the call (the same library and the same type), so a change to the enum or to a
// description reaches the label.
var answerSchemaJSON = sync.OnceValue(func() string {
	schema, err := jsonschema.GenerateSchemaForType(Answer{})
	if err != nil {
		return "schema unavailable: " + err.Error() // never expected; the label would show it
	}
	out, err := json.Marshal(schema)
	if err != nil {
		return "schema unavailable: " + err.Error()
	}
	return string(out)
})

// sampleUserMessage lays out a fixed request that uses every kind of fact, the way the
// model sees it. It is the cheapest way to make a change to the layout of the facts
// (userMessage and the helpers under it) move the label without anyone remembering to.
var sampleUserMessage = sync.OnceValue(func() string {
	r := Request{
		SchemaVersion: SchemaVersion,
		Mode:          ModeShadow,
		Signal: Signal{
			SignalID: "sample", Symbol: "SAMPLE", Direction: "LONG", Timeframe: "15Min",
			Version: "sample", ClosedBarTimestamp: "2026-01-02T15:15:00+00:00",
			ExpiresAt: "2026-01-02T15:45:00+00:00", Confidence: 0.5,
		},
		Rationale:   "sample rationale",
		Technicals:  map[string]any{"close": 10.5, "rsi": 55.5, "trend_up": true},
		RecentBars:  []map[string]any{{"start": "2026-01-02T14:45:00+00:00", "close": 10.4, "volume": 1000}},
		VolumeRatio: 1.25,
		LivePrice:   "10.55",
		Context:     map[string]any{"equity": "100000.00", "open_positions": 1},
		News: []NewsItem{{
			PublishedAt: "2026-01-02T12:00:00+00:00", Source: "sample",
			Headline: "sample headline", Summary: "sample summary", OtherSymbols: 2,
		}},
	}
	return r.userMessage()
})
