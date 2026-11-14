package llm

import (
	"fmt"
	"log/slog"

	"go-monolith/internal/config"
)

// OracleClient picks the model that answers the Oracle.
//
//   - PAID_LLM_ENABLED=false (the default): the model of the chat, as it always was,
//     whatever ORACLE_LLM says (a deepseek there is ignored, with a warning in the log);
//   - PAID_LLM_ENABLED=true: DeepSeek, unless ORACLE_LLM=local keeps the Oracle local.
//
// A setting that cannot be followed stops the start instead of quietly using the other
// model: a typo must not turn into a paid call or into a change of the model under the
// bot's pre-registered judge (it judges one period of (model, prompt label) at a time, and
// the model is in every row of the journal). There is no fallback to the local model when
// the paid one fails or the daily limit is reached: a failure is a HOLD for the bot, as it
// always was.
func OracleClient(cfg *config.Config, local LLMClient, logger *slog.Logger) (LLMClient, error) {
	switch cfg.OracleLLM {
	case "", "local", "deepseek":
	default:
		return nil, fmt.Errorf("ORACLE_LLM=%q is not known: use local or deepseek", cfg.OracleLLM)
	}

	if !cfg.PaidLLMEnabled {
		if cfg.OracleLLM == "deepseek" {
			logger.Warn("ORACLE_LLM=deepseek is ignored: PAID_LLM_ENABLED is not true, the Oracle stays on the local model")
		}
		return local, nil
	}
	if cfg.OracleLLM == "local" {
		return local, nil
	}

	if cfg.DeepSeekAPIKey == "" {
		return nil, fmt.Errorf("PAID_LLM_ENABLED=true needs DEEPSEEK_API_KEY")
	}
	model := cfg.OracleLLMModel
	if model == "" {
		model = DefaultDeepSeekModel
	}
	logger.Warn("the Oracle asks the paid DeepSeek API, not the local model",
		slog.String("model", model), slog.Float64("daily_limit_usd", cfg.PaidLLMDailyUSD))
	return NewDeepSeekClient(cfg.DeepSeekAPIKey, model, cfg.PaidLLMDailyUSD), nil
}
