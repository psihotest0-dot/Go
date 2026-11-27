package api

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// The code review of 2026-11-27: the limits said "characters" and counted bytes, so a message of
// 20 000 Cyrillic characters (40 000 bytes) was refused under a limit of 32 000 characters.
func TestTheLimitsCountCharacters(t *testing.T) {
	assert.NoError(t, validateContent(strings.Repeat("я", 20000)))
	assert.Error(t, validateContent(strings.Repeat("я", maxContentChars+1)))
	assert.NoError(t, validateSessionID(strings.Repeat("я", maxSessionIDChars)))
	assert.Error(t, validateSessionID(strings.Repeat("я", maxSessionIDChars+1)))
}
