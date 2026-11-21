package agent

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The chat of 2026-11-21: "запиши досега извлеченото и после просто дописвай" was answered with
// write_file on a file that existed, three times; each was a replacement the user confirmed, and
// one kept a third of the file. The tool says so to the model when a replacement loses text, and
// the descriptions of the two tools say which one adds.
func TestAReplacementThatLosesTextTellsTheModelToAppend(t *testing.T) {
	e, store := newFilesToolEnv(t)
	_, err := store.Create(e.userID, "a.txt", "първи ред на файла\nвтори ред на файла\nтрети ред на файла")
	require.NoError(t, err)
	ctx, _ := e.turn("s", false)

	res, err := e.callFile(t, ctx, writeFileTool, map[string]interface{}{"name": "a.txt", "content": "нещо съвсем друго"})

	require.NoError(t, err)
	assert.Equal(t, "pending_approval", res["status"])
	message, _ := res["message"].(string)
	assert.Contains(t, message, "NOT saved yet")
	assert.Contains(t, message, "WARNING: the new text keeps only 0% of the lines the file has now")
	assert.Contains(t, message, "call "+appendFileTool+" with only the new part")
	pending := e.pending(t)
	require.Len(t, pending, 1)
	assert.Contains(t, pending[0].Summary, "ВНИМАНИЕ", "the user is told too")
}

func TestAReplacementThatKeepsTheTextHasNoWarning(t *testing.T) {
	e, store := newFilesToolEnv(t)
	_, err := store.Create(e.userID, "a.txt", "първи ред\nвтори ред")
	require.NoError(t, err)
	ctx, _ := e.turn("s", false)

	res, err := e.callFile(t, ctx, writeFileTool, map[string]interface{}{"name": "a.txt", "content": "първи ред\nвтори ред\nтрети ред"})

	require.NoError(t, err)
	message, _ := res["message"].(string)
	assert.Contains(t, message, "NOT saved yet")
	assert.NotContains(t, message, "WARNING")
}

func TestTheToolDescriptionsSayWhichOneAdds(t *testing.T) {
	descriptions := map[string]string{}
	for _, tool := range fileToolDefinitions() {
		descriptions[tool.Function.Name] = tool.Function.Description
	}

	assert.Contains(t, descriptions[writeFileTool], "ADD to a file that exists call "+appendFileTool)
	assert.Contains(t, descriptions[writeFileTool], "old text is lost")
	assert.Contains(t, descriptions[appendFileTool], "END of a file that exists")
	assert.Contains(t, descriptions[appendFileTool], "send only the new part")
}
