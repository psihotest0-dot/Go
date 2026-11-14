package llm

import (
	"bytes"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go-monolith/internal/config"
)

func capturedLogger() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return slog.New(slog.NewTextHandler(&buf, nil)), &buf
}

func TestOracleUsesTheLocalModelUnlessToldOtherwise(t *testing.T) {
	local := &MockLLM{}
	logger, _ := capturedLogger()

	for _, setting := range []string{"", "local"} {
		got, err := OracleClient(&config.Config{OracleLLM: setting, DeepSeekAPIKey: "a-key-that-is-ignored"}, local, logger)

		require.NoError(t, err, setting)
		assert.Same(t, local, got, "ORACLE_LLM=%q: the Oracle asks the same model as the chat, as it always did", setting)
	}
}

func TestOracleOnDeepSeekClient(t *testing.T) {
	local := &MockLLM{}
	logger, logs := capturedLogger()

	got, err := OracleClient(&config.Config{OracleLLM: "deepseek", DeepSeekAPIKey: "secret-key-123", OracleLLMModel: "deepseek-v4-pro"}, local, logger)

	require.NoError(t, err)
	client, ok := got.(*LemonadeClient)
	require.True(t, ok, "a client of its own, not the chat's")
	assert.Equal(t, "deepseek-v4-pro", client.model)
	assert.Equal(t, DeepSeekBaseURL, client.baseURL)
	assert.Contains(t, logs.String(), "paid DeepSeek", "the owner sees in the log that money is spent")
	assert.Contains(t, logs.String(), "deepseek-v4-pro")
	assert.NotContains(t, logs.String(), "secret-key-123", "the key is never logged")
}

func TestOracleOnDeepSeekNeedsTheKey(t *testing.T) {
	logger, _ := capturedLogger()

	_, err := OracleClient(&config.Config{OracleLLM: "deepseek"}, &MockLLM{}, logger)

	require.Error(t, err, "no quiet fall back to the local model: the owner asked for the other one")
	assert.Contains(t, err.Error(), "DEEPSEEK_API_KEY")
}

func TestOracleRefusesAnUnknownSetting(t *testing.T) {
	logger, _ := capturedLogger()

	_, err := OracleClient(&config.Config{OracleLLM: "depseek", DeepSeekAPIKey: "k"}, &MockLLM{}, logger)

	require.Error(t, err, "a typo must not pick a model")
	assert.Contains(t, err.Error(), "depseek")
}
