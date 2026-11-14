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

// The master switch is off by default and then nothing is paid, whatever else is set.
func TestOracleStaysLocalWhileThePaidModelIsOff(t *testing.T) {
	local := &MockLLM{}

	for _, setting := range []string{"", "local", "deepseek"} {
		logger, logs := capturedLogger()
		got, err := OracleClient(&config.Config{OracleLLM: setting, DeepSeekAPIKey: "a-key"}, local, logger)

		require.NoError(t, err, setting)
		assert.Same(t, local, got, "PAID_LLM_ENABLED is false, ORACLE_LLM=%q: the Oracle asks the model of the chat, as it always did", setting)
		if setting == "deepseek" {
			assert.Contains(t, logs.String(), "ignored", "the owner is told that the setting does nothing")
		}
	}
}

func TestTheMasterSwitchSendsTheOracleToDeepSeek(t *testing.T) {
	local := &MockLLM{}
	logger, logs := capturedLogger()

	got, err := OracleClient(&config.Config{PaidLLMEnabled: true, DeepSeekAPIKey: "secret-key-123", PaidLLMDailyUSD: 1.5}, local, logger)

	require.NoError(t, err)
	client, ok := got.(*LemonadeClient)
	require.True(t, ok, "a client of its own, not the chat's")
	assert.Equal(t, DefaultDeepSeekModel, client.model, "the cheap model unless another is named")
	assert.Equal(t, DeepSeekBaseURL, client.baseURL)
	require.NotNil(t, client.Spend(), "a paid client has a daily limit")
	assert.Contains(t, logs.String(), "paid DeepSeek", "the owner sees in the log that money is spent")
	assert.Contains(t, logs.String(), "1.5", "and what the limit is")
	assert.NotContains(t, logs.String(), "secret-key-123", "the key is never logged")
}

func TestTheModelCanBeNamed(t *testing.T) {
	logger, _ := capturedLogger()

	for _, setting := range []string{"", "deepseek"} {
		got, err := OracleClient(&config.Config{PaidLLMEnabled: true, OracleLLM: setting, OracleLLMModel: "deepseek-v4-pro", DeepSeekAPIKey: "k", PaidLLMDailyUSD: 1}, &MockLLM{}, logger)

		require.NoError(t, err)
		assert.Equal(t, "deepseek-v4-pro", got.(*LemonadeClient).model)
	}
}

// ORACLE_LLM=local keeps the Oracle on the local model even when the paid one is on (for the
// day the chat uses it and the Oracle should not).
func TestTheOracleCanStayLocalWhilePaidIsOn(t *testing.T) {
	local := &MockLLM{}
	logger, _ := capturedLogger()

	got, err := OracleClient(&config.Config{PaidLLMEnabled: true, OracleLLM: "local", DeepSeekAPIKey: "k"}, local, logger)

	require.NoError(t, err)
	assert.Same(t, local, got)
}

func TestThePaidModelNeedsTheKey(t *testing.T) {
	logger, _ := capturedLogger()

	_, err := OracleClient(&config.Config{PaidLLMEnabled: true}, &MockLLM{}, logger)

	require.Error(t, err, "no quiet fall back to the local model: the owner asked for the other one")
	assert.Contains(t, err.Error(), "DEEPSEEK_API_KEY")
}

func TestNoKeyIsNeededWhileThePaidModelIsOff(t *testing.T) {
	local := &MockLLM{}
	logger, _ := capturedLogger()

	got, err := OracleClient(&config.Config{}, local, logger)

	require.NoError(t, err)
	assert.Same(t, local, got)
}

func TestAnUnknownSettingStopsTheStartEvenWhileOff(t *testing.T) {
	logger, _ := capturedLogger()

	for _, enabled := range []bool{false, true} {
		_, err := OracleClient(&config.Config{PaidLLMEnabled: enabled, OracleLLM: "depseek", DeepSeekAPIKey: "k"}, &MockLLM{}, logger)

		require.Error(t, err, "a typo must not pick a model (enabled=%v)", enabled)
		assert.Contains(t, err.Error(), "depseek")
	}
}
