package api

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseMessagesLimit(t *testing.T) {
	tests := []struct {
		name        string
		raw         string
		expected    int32
		expectError bool
	}{
		{name: "absent uses the default", raw: "", expected: defaultMessagesLimit},
		{name: "explicit value is honoured", raw: "25", expected: 25},
		{name: "oversized value is capped", raw: "100000", expected: maxMessagesLimit},
		{name: "zero is rejected", raw: "0", expectError: true},
		{name: "negative is rejected", raw: "-5", expectError: true},
		{name: "garbage is rejected", raw: "abc", expectError: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			limit, err := parseMessagesLimit(tt.raw)
			if tt.expectError {
				require.Error(t, err)
				return
			}

			require.NoError(t, err)
			require.Equal(t, tt.expected, limit)
			// A LIMIT of zero would make the query return an empty page, which
			// is the bug this helper exists to prevent.
			require.Positive(t, limit)
		})
	}
}
