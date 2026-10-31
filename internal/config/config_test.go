package config

import "testing"

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
