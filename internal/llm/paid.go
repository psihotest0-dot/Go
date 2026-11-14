package llm

import (
	"fmt"
	"log/slog"

	"go-monolith/internal/config"
)

// Paid is the paid provider of this process (DeepSeek): one key and ONE daily limit that
// every purpose shares - the Oracle, the questions to the library and the chat that the
// owner sends to the cloud - so two clients cannot each spend the limit.
type Paid struct {
	key   string
	guard *SpendGuard
}

// NewPaid returns the paid provider, or nil when it is off (PAID_LLM_ENABLED is not true):
// then nothing is paid, whatever ORACLE_LLM and LIBRARY_LLM say (a deepseek there is
// ignored, with a warning in the log). An unknown value of those two stops the start even
// while it is off - a typo must not pick a model - and so does a missing key when it is on.
func NewPaid(cfg *config.Config, logger *slog.Logger) (*Paid, error) {
	for name, value := range map[string]string{"ORACLE_LLM": cfg.OracleLLM, "LIBRARY_LLM": cfg.LibraryLLM} {
		switch value {
		case "", "local", "deepseek":
		default:
			return nil, fmt.Errorf("%s=%q is not known: use local or deepseek", name, value)
		}
	}

	if !cfg.PaidLLMEnabled {
		if cfg.OracleLLM == "deepseek" || cfg.LibraryLLM == "deepseek" {
			logger.Warn("ORACLE_LLM / LIBRARY_LLM = deepseek is ignored: PAID_LLM_ENABLED is not true, everything stays on the local model")
		}
		return nil, nil
	}
	if cfg.DeepSeekAPIKey == "" {
		return nil, fmt.Errorf("PAID_LLM_ENABLED=true needs DEEPSEEK_API_KEY")
	}
	return &Paid{key: cfg.DeepSeekAPIKey, guard: NewSpendGuard(cfg.PaidLLMDailyUSD)}, nil
}

// Guard is the daily limit, shared by every client of the provider.
func (p *Paid) Guard() *SpendGuard { return p.guard }

// Key is the key of the account (for the balance watcher; it is never logged).
func (p *Paid) Key() string { return p.key }

// Client returns a client of the provider for the model (empty: the cheap one). Clients
// differ in the model only: the key and the limit are the provider's.
func (p *Paid) Client(model string) *LemonadeClient {
	return NewLemonadeClientWithOptions(deepSeekOptions(p.key, model, p.guard))
}

// OracleClient picks the model that answers the Oracle.
//
//   - the paid provider is off (paid == nil): the model of the chat, as it always was;
//   - it is on: DeepSeek, unless ORACLE_LLM=local keeps the Oracle local.
//
// There is no fallback to the local model when the paid one fails or the daily limit is
// reached: a failure is a HOLD for the bot, as it always was, and the model is in every row
// of the journal, which the bot's pre-registered judge reads (it judges one period of
// (model, prompt label) at a time).
func OracleClient(cfg *config.Config, local LLMClient, paid *Paid, logger *slog.Logger) LLMClient {
	if paid == nil || cfg.OracleLLM == "local" {
		return local
	}
	model := cfg.OracleLLMModel
	if model == "" {
		model = DefaultDeepSeekModel
	}
	logger.Warn("the Oracle asks the paid DeepSeek API, not the local model",
		slog.String("model", model), slog.Float64("daily_limit_usd", cfg.PaidLLMDailyUSD))
	return paid.Client(model)
}

// ChatClient is the client the chat uses when it goes to the cloud (the questions to the
// library, and a conversation the owner sent there), or nil when the provider is off. The
// second result says whether the questions to the library go there on their own
// (LIBRARY_LLM is not local).
func ChatClient(cfg *config.Config, paid *Paid, logger *slog.Logger) (client LLMClient, model string, library bool) {
	if paid == nil {
		return nil, "", false
	}
	model = cfg.PaidLLMModel
	if model == "" {
		model = DefaultDeepSeekModel
	}
	library = cfg.LibraryLLM != "local"
	logger.Warn("the chat can use the paid DeepSeek API",
		slog.String("model", model), slog.Bool("library_questions_go_there", library),
		slog.Float64("daily_limit_usd", cfg.PaidLLMDailyUSD))
	return paid.Client(model), model, library
}
