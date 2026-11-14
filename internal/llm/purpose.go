package llm

import (
	"fmt"
	"log/slog"

	"go-monolith/internal/config"
)

// OracleClient picks the model that answers the Oracle: the same client as the chat
// (ORACLE_LLM unset or "local", what it always was), or DeepSeek (ORACLE_LLM=deepseek).
//
// A setting that cannot be followed stops the start instead of quietly using the other
// model: a typo must not turn into a paid call or into a change of the model under the
// bot's pre-registered judge (it judges one period of (model, prompt label) at a time, and
// the model is in every row of the journal). There is no fallback to the local model when
// the paid one fails: a failure is a HOLD for the bot, as it always was.
func OracleClient(cfg *config.Config, local LLMClient, logger *slog.Logger) (LLMClient, error) {
	switch cfg.OracleLLM {
	case "", "local":
		return local, nil
	case "deepseek":
		if cfg.DeepSeekAPIKey == "" {
			return nil, fmt.Errorf("ORACLE_LLM=deepseek needs DEEPSEEK_API_KEY")
		}
		model := cfg.OracleLLMModel
		if model == "" {
			model = DefaultDeepSeekModel
		}
		logger.Warn("the Oracle asks the paid DeepSeek API, not the local model", slog.String("model", model))
		return NewDeepSeekClient(cfg.DeepSeekAPIKey, model), nil
	default:
		return nil, fmt.Errorf("ORACLE_LLM=%q is not known: use local or deepseek", cfg.OracleLLM)
	}
}
