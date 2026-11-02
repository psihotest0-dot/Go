package config

import (
	"strconv"
	"testing"
	"time"
)

func TestLoadTelegramAllowedChatIDs(t *testing.T) {
	tests := []struct {
		name string
		env  string
		want []int64
	}{
		{name: "unset allows nobody", env: "", want: []int64{}},
		{name: "single id", env: "123456789", want: []int64{123456789}},
		{name: "comma separated with spaces", env: "1, 2 ,3", want: []int64{1, 2, 3}},
		{name: "space and semicolon separators", env: "1 2;3", want: []int64{1, 2, 3}},
		{name: "negative group id", env: "-1001234567890", want: []int64{-1001234567890}},
		{name: "invalid entries are skipped", env: "1,abc,2,,3x", want: []int64{1, 2}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("TELEGRAM_ALLOWED_CHAT_IDS", tt.env)
			got := Load().TelegramAllowedChatIDs
			if len(got) != len(tt.want) {
				t.Fatalf("TelegramAllowedChatIDs = %v, want %v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Fatalf("TelegramAllowedChatIDs = %v, want %v", got, tt.want)
				}
			}
		})
	}
}

func TestLoadMaxUploadBytes(t *testing.T) {
	tests := []struct {
		name string
		env  string
		want int64
	}{
		{name: "default is 100 MB", env: "", want: 100 << 20},
		{name: "explicit value", env: "250", want: 250 << 20},
		{name: "surrounding space is trimmed", env: " 50 ", want: 50 << 20},
		{name: "not a number falls back", env: "lots", want: 100 << 20},
		{name: "zero falls back", env: "0", want: 100 << 20},
		{name: "negative falls back", env: "-5", want: 100 << 20},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("MAX_UPLOAD_MB", tt.env)
			if got := Load().MaxUploadBytes; got != tt.want {
				t.Fatalf("MaxUploadBytes = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestLoadListenAddr(t *testing.T) {
	tests := []struct {
		name string
		env  string
		want string
	}{
		{name: "default is loopback only", env: "", want: "127.0.0.1:8080"},
		{name: "blank falls back to default", env: "   ", want: "127.0.0.1:8080"},
		{name: "explicit override", env: "0.0.0.0:9090", want: "0.0.0.0:9090"},
		{name: "surrounding space is trimmed", env: " 127.0.0.1:9000 ", want: "127.0.0.1:9000"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("LISTEN_ADDR", tt.env)
			if got := Load().ListenAddr; got != tt.want {
				t.Fatalf("ListenAddr = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestLoadLLMSlots(t *testing.T) {
	tests := []struct {
		name           string
		slots          string
		backgroundSlot string
		wantSlots      int
		wantBackground int
	}{
		{name: "defaults match the two-slot server", slots: "", backgroundSlot: "", wantSlots: 2, wantBackground: 1},
		{name: "explicit values", slots: "4", backgroundSlot: "2", wantSlots: 4, wantBackground: 2},
		{name: "surrounding space is trimmed", slots: " 3 ", backgroundSlot: " 1 ", wantSlots: 3, wantBackground: 1},
		{name: "not a number falls back", slots: "many", backgroundSlot: "some", wantSlots: 2, wantBackground: 1},
		{name: "zero falls back", slots: "0", backgroundSlot: "0", wantSlots: 2, wantBackground: 1},
		{name: "negative falls back", slots: "-1", backgroundSlot: "-1", wantSlots: 2, wantBackground: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("LLM_SLOTS", tt.slots)
			t.Setenv("LLM_BACKGROUND_SLOTS", tt.backgroundSlot)
			cfg := Load()
			if cfg.LLMSlots != tt.wantSlots || cfg.LLMBackgroundSlots != tt.wantBackground {
				t.Fatalf("slots = %d/%d, want %d/%d", cfg.LLMSlots, cfg.LLMBackgroundSlots, tt.wantSlots, tt.wantBackground)
			}
		})
	}
}

func TestLoadLLMModel(t *testing.T) {
	t.Setenv("LLM_MODEL", "")
	if got := Load().LLMModel; got != "" {
		t.Fatalf("LLMModel = %q, want empty so the client asks the server", got)
	}

	t.Setenv("LLM_MODEL", "  Qwen3.5-9B-GGUF ")
	if got := Load().LLMModel; got != "Qwen3.5-9B-GGUF" {
		t.Fatalf("LLMModel = %q, want the trimmed value", got)
	}
}

func TestLoadLLMBudgets(t *testing.T) {
	names := []string{"LLM_CONTEXT_TOKENS", "LLM_ANSWER_TOKENS", "LLM_PROMPT_CONTEXT_TOKENS",
		"LLM_HISTORY_TOKENS", "LLM_HISTORY_MESSAGES", "LLM_TOOL_RESULT_TOKENS"}

	for _, name := range names {
		t.Setenv(name, "")
	}
	cfg := Load()
	for name, got := range map[string]int{
		"LLM_CONTEXT_TOKENS": cfg.LLMContextTokens, "LLM_ANSWER_TOKENS": cfg.LLMAnswerTokens,
		"LLM_PROMPT_CONTEXT_TOKENS": cfg.LLMPromptContextTokens, "LLM_HISTORY_TOKENS": cfg.LLMHistoryTokens,
		"LLM_HISTORY_MESSAGES": cfg.LLMHistoryMessages, "LLM_TOOL_RESULT_TOKENS": cfg.LLMToolResultTokens,
	} {
		if got != 0 {
			t.Fatalf("%s unset = %d, want 0 (derive it)", name, got)
		}
	}

	for i, name := range names {
		t.Setenv(name, " "+strconv.Itoa(100*(i+1))+" ")
	}
	cfg = Load()
	want := []int{100, 200, 300, 400, 500, 600}
	got := []int{cfg.LLMContextTokens, cfg.LLMAnswerTokens, cfg.LLMPromptContextTokens,
		cfg.LLMHistoryTokens, cfg.LLMHistoryMessages, cfg.LLMToolResultTokens}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%s = %d, want %d", names[i], got[i], want[i])
		}
	}

	for _, name := range names {
		t.Setenv(name, "-3")
	}
	if cfg = Load(); cfg.LLMContextTokens != 0 || cfg.LLMHistoryMessages != 0 {
		t.Fatalf("invalid values must fall back to 0, got context=%d messages=%d", cfg.LLMContextTokens, cfg.LLMHistoryMessages)
	}
}

func TestLoadLLMEnableThinking(t *testing.T) {
	tests := []struct {
		env  string
		want bool
	}{
		{env: "", want: false},
		{env: "true", want: true},
		{env: "TRUE", want: true},
		{env: "1", want: true},
		{env: " true ", want: true},
		{env: "false", want: false},
		{env: "False", want: false},
		{env: "0", want: false},
		{env: " false ", want: false},
		{env: "maybe", want: false},
	}
	for _, tt := range tests {
		t.Run("["+tt.env+"]", func(t *testing.T) {
			t.Setenv("LLM_ENABLE_THINKING", tt.env)
			if got := Load().LLMEnableThinking; got != tt.want {
				t.Fatalf("LLMEnableThinking = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestLoadLLMSamplingDefaultsAndRanges(t *testing.T) {
	t.Setenv("LLM_TEMPERATURE", "")
	t.Setenv("LLM_TOP_P", "")
	cfg := Load()
	if cfg.LLMTemperature != 0.7 || cfg.LLMTopP != 0.8 {
		t.Fatalf("defaults must be temperature 0.7 and top_p 0.8, got %v and %v", cfg.LLMTemperature, cfg.LLMTopP)
	}

	tests := []struct {
		name, env, value string
		get              func(*Config) float32
		want             float32
	}{
		{"temperature set", "LLM_TEMPERATURE", "0.4", func(c *Config) float32 { return c.LLMTemperature }, 0.4},
		{"temperature zero is allowed", "LLM_TEMPERATURE", "0", func(c *Config) float32 { return c.LLMTemperature }, 0},
		{"temperature too high", "LLM_TEMPERATURE", "3", func(c *Config) float32 { return c.LLMTemperature }, 0.7},
		{"temperature negative", "LLM_TEMPERATURE", "-1", func(c *Config) float32 { return c.LLMTemperature }, 0.7},
		{"temperature garbage", "LLM_TEMPERATURE", "топло", func(c *Config) float32 { return c.LLMTemperature }, 0.7},
		{"top_p set", "LLM_TOP_P", "0.95", func(c *Config) float32 { return c.LLMTopP }, 0.95},
		{"top_p zero is not a nucleus", "LLM_TOP_P", "0", func(c *Config) float32 { return c.LLMTopP }, 0.8},
		{"top_p above one", "LLM_TOP_P", "1.5", func(c *Config) float32 { return c.LLMTopP }, 0.8},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(tt.env, tt.value)
			if got := tt.get(Load()); got != tt.want {
				t.Fatalf("%s=%q gave %v, want %v", tt.env, tt.value, got, tt.want)
			}
		})
	}
}

func TestLoadLLMPresencePenalty(t *testing.T) {
	tests := []struct {
		env  string
		want float32
	}{
		{env: "", want: 0},
		{env: "0", want: 0},
		{env: "0.5", want: 0.5},
		{env: " 1.5 ", want: 1.5},
		{env: "2", want: 2},
		{env: "-0.5", want: 0},
		{env: "2.1", want: 0},
		{env: "NaN", want: 0},
		{env: "Inf", want: 0},
		{env: "много", want: 0},
	}
	for _, tt := range tests {
		t.Run("["+tt.env+"]", func(t *testing.T) {
			t.Setenv("LLM_PRESENCE_PENALTY", tt.env)
			if got := Load().LLMPresencePenalty; got != tt.want {
				t.Fatalf("LLMPresencePenalty = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestLoadEmbedderTimeout(t *testing.T) {
	tests := []struct {
		name string
		env  string
		want time.Duration
	}{
		{name: "default", env: "", want: 30 * time.Second},
		{name: "explicit", env: "90", want: 90 * time.Second},
		{name: "zero is ignored", env: "0", want: 30 * time.Second},
		{name: "negative is ignored", env: "-5", want: 30 * time.Second},
		{name: "not a number is ignored", env: "soon", want: 30 * time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("EMBEDDER_TIMEOUT_SECONDS", tt.env)
			if got := Load().EmbedderTimeout; got != tt.want {
				t.Fatalf("EmbedderTimeout = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestLoadAutoMigrate(t *testing.T) {
	tests := []struct {
		name string
		env  string
		want bool
	}{
		{name: "on by default", env: "", want: true},
		{name: "false", env: "false", want: false},
		{name: "zero", env: "0", want: false},
		{name: "explicit true", env: "true", want: true},
		{name: "any case", env: "FALSE", want: false},
		{name: "garbage keeps the default", env: "maybe", want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("AUTO_MIGRATE", tt.env)
			if got := Load().AutoMigrate; got != tt.want {
				t.Fatalf("AutoMigrate = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestLoadKeepOriginals(t *testing.T) {
	tests := []struct {
		name string
		env  string
		want bool
	}{
		{name: "originals are kept by default", env: "", want: true},
		{name: "false deletes them after a good ingest", env: "false", want: false},
		{name: "zero", env: "0", want: false},
		{name: "any case", env: "FALSE", want: false},
		{name: "explicit true", env: "true", want: true},
		// A typo must not start deleting the only copy of a book.
		{name: "garbage keeps the default", env: "nope", want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("KEEP_ORIGINALS", tt.env)
			if got := Load().KeepOriginals; got != tt.want {
				t.Fatalf("KeepOriginals = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestLoadLogMaxMB(t *testing.T) {
	tests := []struct {
		name string
		env  string
		want int
	}{
		{name: "ten megabytes by default", env: "", want: 10},
		{name: "custom", env: "25", want: 25},
		{name: "zero is not a limit", env: "0", want: 10},
		{name: "negative", env: "-5", want: 10},
		{name: "garbage", env: "big", want: 10},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("LOG_MAX_MB", tt.env)
			if got := Load().LogMaxMB; got != tt.want {
				t.Fatalf("LogMaxMB = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestLoadMetricsLogMaxMB(t *testing.T) {
	tests := []struct {
		name string
		env  string
		want int
	}{
		{name: "five megabytes by default", env: "", want: 5},
		{name: "custom", env: "2", want: 2},
		{name: "zero is not a limit", env: "0", want: 5},
		{name: "negative", env: "-1", want: 5},
		{name: "garbage", env: "много", want: 5},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("METRICS_LOG_MAX_MB", tt.env)
			if got := Load().MetricsLogMaxMB; got != tt.want {
				t.Fatalf("MetricsLogMaxMB = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestFileCategories(t *testing.T) {
	for _, name := range []string{"medicine", "economics", "programming", "law", "psychology", "literature", "philosophy", "self_help", "history", "general"} {
		if !IsFileCategory(name) {
			t.Errorf("%q should be a category", name)
		}
	}
	for _, name := range []string{"", "General", " general", "cooking", "general;", "medicine,law"} {
		if IsFileCategory(name) {
			t.Errorf("%q should not be a category", name)
		}
	}
	if !IsFileCategory(DefaultFileCategory) {
		t.Errorf("the default category %q must be one of FileCategories", DefaultFileCategory)
	}
}
