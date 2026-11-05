// Package oracle is the second opinion the trading bot asks of the model: given one
// signal of its strategy (numbers and a few cleaned news headlines), the model says
// APPROVE, VETO or HOLD and gives a short reason.
//
// The model sees nothing else. It has no tools, no memory, no history and no access
// to the database, and it can only answer one of three words, so the worst a
// planted sentence in a headline can do is turn one verdict into another; the bot's
// risk engine still decides every order. Every call is written to the
// oracle_evaluations table, which in shadow mode (the bot ignores the verdict) is
// the whole result.
package oracle

import (
	"encoding/json"
	"time"
)

// The three things the Oracle can say. Anything else from the model is an unusable
// reply, never a verdict.
const (
	VerdictApprove = "APPROVE"
	VerdictVeto    = "VETO"
	VerdictHold    = "HOLD"
)

// SchemaVersion is the version of the request body this code understands.
const SchemaVersion = 1

// Modes the bot may report. The Oracle does not act on it; it is kept in the
// journal so that shadow answers and live ones can be told apart afterwards.
const (
	ModeShadow = "shadow"
	ModeActive = "active"
)

// Journal statuses.
const (
	StatusAnswered = "answered"
	StatusFailed   = "failed"
)

// Answer is what the model is asked to return. The enum tag becomes part of the JSON
// schema the model server enforces, so the verdict cannot be anything else.
type Answer struct {
	Verdict string `json:"verdict" enum:"APPROVE,VETO,HOLD" description:"APPROVE: the facts support the signal now. VETO: they speak against it. HOLD: unclear, ask again later."`
	Reason  string `json:"reason" description:"One short sentence in Bulgarian that names the main fact behind the verdict."`
}

// Request is the body of POST /oracle/evaluate: the bot's OracleRequest plus a
// schema version and its mode. Unknown fields are ignored, so the bot can add a
// field without breaking this side; the facts are shown to the model generically,
// not field by field.
type Request struct {
	SchemaVersion int              `json:"schema_version"`
	Mode          string           `json:"mode"`
	Signal        Signal           `json:"signal"`
	Rationale     string           `json:"rationale"`
	Technicals    map[string]any   `json:"technicals"`
	RecentBars    []map[string]any `json:"recent_bars"`
	VolumeRatio   any              `json:"volume_ratio"`
	LivePrice     any              `json:"live_price"`
	Context       map[string]any   `json:"context"`
	News          []NewsItem       `json:"news"`
}

// Signal is the strategy signal being judged.
type Signal struct {
	SignalID           string `json:"signal_id"`
	Symbol             string `json:"symbol"`
	Direction          string `json:"direction"`
	Timeframe          string `json:"timeframe"`
	Version            string `json:"version"`
	ClosedBarTimestamp string `json:"closed_bar_timestamp"`
	ExpiresAt          string `json:"expires_at"`
	Confidence         any    `json:"confidence"`
}

// NewsItem is one article about the stock, already cleaned by the bot; it is
// cleaned and cut again here, because this is the one piece of outside text.
type NewsItem struct {
	ArticleID    any    `json:"article_id"` // for the record; the model does not see it
	PublishedAt  string `json:"published_at"`
	Source       string `json:"source"`
	Headline     string `json:"headline"`
	Summary      string `json:"summary"`
	OtherSymbols int    `json:"other_symbols"`
}

// Result is the answer to the bot.
type Result struct {
	Verdict   string `json:"verdict"`
	Reason    string `json:"reason"`
	RequestID string `json:"request_id"`
	Model     string `json:"model"`
	// Cached is true when the verdict is one given a moment ago for the same
	// signal (asked again, or asked while the first question was still running).
	Cached    bool  `json:"cached"`
	LatencyMS int64 `json:"latency_ms"`
}

// Evaluation is one row of the journal.
type Evaluation struct {
	ID               int64           `json:"id"`
	CreatedAt        time.Time       `json:"created_at"`
	RequestID        string          `json:"request_id"`
	SignalID         string          `json:"signal_id"`
	Symbol           string          `json:"symbol"`
	Mode             string          `json:"mode"`
	Status           string          `json:"status"`
	Verdict          string          `json:"verdict,omitempty"`
	Reason           string          `json:"reason"`
	Model            string          `json:"model"`
	Attempts         int             `json:"attempts"`
	FinishReason     string          `json:"finish_reason"`
	PromptTokens     int             `json:"prompt_tokens"`
	CompletionTokens int             `json:"completion_tokens"`
	LatencyMS        int             `json:"latency_ms"`
	Error            string          `json:"error,omitempty"`
	Request          json.RawMessage `json:"-"` // only written; the list leaves it out
}
