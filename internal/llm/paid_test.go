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
func TestNothingIsPaidWhileTheMasterSwitchIsOff(t *testing.T) {
	for _, setting := range []string{"", "local", "deepseek"} {
		logger, logs := capturedLogger()
		cfg := &config.Config{OracleLLM: setting, LibraryLLM: setting, DeepSeekAPIKey: "a-key"}

		paid, err := NewPaid(cfg, logger)

		require.NoError(t, err, setting)
		assert.Nil(t, paid, "PAID_LLM_ENABLED is false, ORACLE_LLM=LIBRARY_LLM=%q: no paid provider", setting)
		if setting == "deepseek" {
			assert.Contains(t, logs.String(), "ignored", "the owner is told that the setting does nothing")
		}

		local := &MockLLM{}
		assert.Same(t, local, OracleClient(cfg, local, paid, logger), "the Oracle asks the model of the chat, as it always did")
		client, _, library := ChatClient(cfg, paid, logger)
		assert.Nil(t, client)
		assert.False(t, library)
	}
}

func TestTheMasterSwitchOpensTheProvider(t *testing.T) {
	logger, logs := capturedLogger()
	cfg := &config.Config{PaidLLMEnabled: true, DeepSeekAPIKey: "secret-key-123", PaidLLMDailyUSD: 1.5}

	paid, err := NewPaid(cfg, logger)

	require.NoError(t, err)
	require.NotNil(t, paid)
	require.NotNil(t, paid.Guard())

	local := &MockLLM{}
	oracle := OracleClient(cfg, local, paid, logger)
	client, ok := oracle.(*LemonadeClient)
	require.True(t, ok, "the Oracle gets a client of its own, not the chat's")
	assert.Equal(t, DefaultDeepSeekModel, client.model, "the cheap model unless another is named")
	assert.Equal(t, DeepSeekBaseURL, client.baseURL)

	chat, model, library := ChatClient(cfg, paid, logger)
	require.NotNil(t, chat)
	assert.Equal(t, DefaultDeepSeekModel, model)
	assert.True(t, library, "the questions to the library follow the provider unless LIBRARY_LLM=local")

	assert.Contains(t, logs.String(), "paid DeepSeek", "the owner sees in the log that money is spent")
	assert.Contains(t, logs.String(), "1.5", "and what the limit is")
	assert.NotContains(t, logs.String(), "secret-key-123", "the key is never logged")
}

// One limit for everything that uses the account: two clients cannot each spend it.
func TestTheClientsOfTheProviderShareOneLimit(t *testing.T) {
	logger, _ := capturedLogger()
	paid, err := NewPaid(&config.Config{PaidLLMEnabled: true, DeepSeekAPIKey: "k", PaidLLMDailyUSD: 1}, logger)
	require.NoError(t, err)

	oracle := OracleClient(&config.Config{PaidLLMEnabled: true, DeepSeekAPIKey: "k"}, &MockLLM{}, paid, logger).(*LemonadeClient)
	chat, _, _ := ChatClient(&config.Config{PaidLLMEnabled: true, DeepSeekAPIKey: "k"}, paid, logger)

	assert.Same(t, paid.Guard(), oracle.Spend())
	assert.Same(t, paid.Guard(), chat.(*LemonadeClient).Spend(), "the chat and the Oracle count into the same day")
}

func TestTheModelsCanBeNamed(t *testing.T) {
	logger, _ := capturedLogger()
	cfg := &config.Config{PaidLLMEnabled: true, DeepSeekAPIKey: "k", PaidLLMDailyUSD: 1,
		OracleLLMModel: "deepseek-v4-pro", PaidLLMModel: "deepseek-flash"}
	paid, err := NewPaid(cfg, logger)
	require.NoError(t, err)

	assert.Equal(t, "deepseek-v4-pro", OracleClient(cfg, &MockLLM{}, paid, logger).(*LemonadeClient).model)
	chat, model, _ := ChatClient(cfg, paid, logger)
	assert.Equal(t, "deepseek-flash", model)
	assert.Equal(t, "deepseek-flash", chat.(*LemonadeClient).model)
}

// ORACLE_LLM=local keeps the Oracle on the local model even when the paid one is on (while the
// bot's judge is still on the first period), and LIBRARY_LLM=local does the same for the library.
func TestEachPurposeCanStayLocalWhilePaidIsOn(t *testing.T) {
	logger, _ := capturedLogger()
	cfg := &config.Config{PaidLLMEnabled: true, OracleLLM: "local", LibraryLLM: "local", DeepSeekAPIKey: "k", PaidLLMDailyUSD: 1}
	paid, err := NewPaid(cfg, logger)
	require.NoError(t, err)

	local := &MockLLM{}
	assert.Same(t, local, OracleClient(cfg, local, paid, logger), "the Oracle is untouched")
	chat, _, library := ChatClient(cfg, paid, logger)
	assert.NotNil(t, chat, "the button of the client still has a cloud to send to")
	assert.False(t, library, "but the library questions do not go there on their own")
}

func TestThePaidProviderNeedsTheKey(t *testing.T) {
	logger, _ := capturedLogger()

	_, err := NewPaid(&config.Config{PaidLLMEnabled: true}, logger)

	require.Error(t, err, "no quiet fall back to the local model: the owner asked for the other one")
	assert.Contains(t, err.Error(), "DEEPSEEK_API_KEY")
}

func TestAnUnknownSettingStopsTheStartEvenWhileOff(t *testing.T) {
	logger, _ := capturedLogger()

	for _, enabled := range []bool{false, true} {
		for _, cfg := range []*config.Config{
			{PaidLLMEnabled: enabled, OracleLLM: "depseek", DeepSeekAPIKey: "k"},
			{PaidLLMEnabled: enabled, LibraryLLM: "cloud", DeepSeekAPIKey: "k"},
			{PaidLLMEnabled: enabled, ExplainLLM: "cloud", DeepSeekAPIKey: "k"},
			{PaidLLMEnabled: enabled, TelegramLLM: "lokal", DeepSeekAPIKey: "k"},
			{PaidLLMEnabled: enabled, TelegramLLM: "deepseek", DeepSeekAPIKey: "k"},
		} {
			_, err := NewPaid(cfg, logger)

			require.Error(t, err, "a typo must not pick a model (enabled=%v)", enabled)
		}
	}
}

// TELEGRAM_LLM takes auto (or nothing) and local; both leave the provider as it is.
func TestTelegramSettingTakesAutoAndLocal(t *testing.T) {
	logger, _ := capturedLogger()

	for _, setting := range []string{"", "auto", "local"} {
		paid, err := NewPaid(&config.Config{PaidLLMEnabled: true, TelegramLLM: setting, DeepSeekAPIKey: "k", PaidLLMDailyUSD: 1}, logger)

		require.NoError(t, err, "TELEGRAM_LLM=%q", setting)
		require.NotNil(t, paid)
	}
}
