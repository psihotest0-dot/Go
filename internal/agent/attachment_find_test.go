package agent

import (
	"fmt"
	"go-monolith/internal/attachments"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func handlersFile() string {
	var b strings.Builder
	for i := 1; i <= 300; i++ {
		fmt.Fprintf(&b, "func handler%d() int { return %d }\n", i, i)
	}
	b.WriteString("func special() int { return 4242 } // the special one\n")
	return b.String()
}

func TestReadAttachmentFindsALineInABigFile(t *testing.T) {
	e := newAttachmentEnv(t)
	e.attach(t, e.userID, "find-chat", "handlers.go", handlersFile())
	ctx, turn := e.turn("find-chat", false)

	res := e.read(t, ctx, map[string]interface{}{"name": "handlers.go", "find": "SPECIAL"})

	assert.Equal(t, "success", res["status"])
	assert.EqualValues(t, 301, res["total_lines"])
	assert.EqualValues(t, 1, res["total_matches"])
	matches, ok := res["matches"].([]attachments.Match)
	require.True(t, ok, "%T", res["matches"])
	require.Len(t, matches, 1)
	assert.Equal(t, 301, matches[0].Line)
	assert.Contains(t, matches[0].Text, "return 4242")
	assert.NotContains(t, res, "content", "a search returns lines, not a reading")
	assert.Equal(t, []string{sourceAttachments}, turn.externalSources())
}

func TestReadAttachmentSaysSoWhenNothingMatchesOrTooMuchDoes(t *testing.T) {
	e := newAttachmentEnv(t)
	e.attach(t, e.userID, "find-chat-2", "handlers.go", handlersFile())
	ctx, _ := e.turn("find-chat-2", false)

	none := e.read(t, ctx, map[string]interface{}{"name": "handlers.go", "find": "nothing like this"})
	assert.EqualValues(t, 0, none["total_matches"])
	assert.Contains(t, none["message"], "No line contains that text")

	many := e.read(t, ctx, map[string]interface{}{"name": "handlers.go", "find": "handler"})
	assert.EqualValues(t, 300, many["total_matches"])
	assert.Len(t, many["matches"], 40, "only the first forty")
	assert.Contains(t, many["message"], "Only the first 40 of 300")
}

func TestReadAttachmentWithoutFindStillReadsInParts(t *testing.T) {
	e := newAttachmentEnv(t)
	e.attach(t, e.userID, "find-chat-3", "handlers.go", handlersFile())
	ctx, _ := e.turn("find-chat-3", false)

	res := e.read(t, ctx, map[string]interface{}{"name": "handlers.go", "start_line": float64(299)})

	assert.EqualValues(t, 301, res["end_line"])
	assert.Contains(t, res["content"], " 301| func special()")
	assert.NotContains(t, res, "matches")
}
