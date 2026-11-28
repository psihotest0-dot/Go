package agent

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The live check of 2026-11-28: the model read the two files of the folder and called write_file, which
// left the write for the person to confirm; the answer said so - and got "не е извикан инструмент за
// файлове" under it, four times.
func TestAWriteLeftForConfirmationIsNoFalseClaim(t *testing.T) {
	var claim answerCheck
	for _, c := range answerChecks {
		if c.name == "claim" {
			claim = c
		}
	}
	require.NotNil(t, claim.note)
	const answer = "Подготвих кратко резюме на двата файла в нов файл „резюмета.md“. Можеш да го потвърдиш, за да го запиша окончателно."

	_, turn := withTurn(context.Background(), "s", false)
	assert.Equal(t, writeCorrection, claim.note(context.Background(), nil, answerFacts{content: answer, turn: turn}),
		"with nothing written and nothing proposed it is a false claim")

	turn.markWriteProposed()
	assert.Empty(t, claim.note(context.Background(), nil, answerFacts{content: answer, turn: turn}),
		"a write waits for the person: the answer says what is true")
}
