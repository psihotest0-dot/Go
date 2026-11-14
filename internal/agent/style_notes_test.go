package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go-monolith/internal/db"
	"go-monolith/internal/prompts"
	"go-monolith/internal/proposals"
)

// A note on how to talk is in every prompt, so the tool only proposes it, like a
// rule (plan item P42).
func TestSaveStyleNoteOnlyProposes(t *testing.T) {
	e := newToolEnv(t)
	ctx, turn := e.turn("sess-1", false)

	res, err := e.reg.ExecuteTool(ctx, styleNoteTool, map[string]interface{}{
		"key": "humour", "value": "Шегува се — отговаряй с шега",
	})
	require.NoError(t, err)

	out := resultMap(t, res)
	assert.Equal(t, "pending_approval", out["status"])
	notes, err := e.db.Queries.GetStyleNotes(context.Background(), e.userID)
	require.NoError(t, err)
	assert.Empty(t, notes, "the note must not reach the prompt before the user agrees")

	pending := e.pending(t)
	require.Len(t, pending, 1)
	assert.Equal(t, proposals.KindSaveStyleNote, pending[0].Kind)
	assert.Equal(t, "humour", pending[0].Key)
	require.Len(t, turn.madeProposals(), 1, "the turn reports it to the client")
}

func TestSaveStyleNoteNeedsAKeyAndAValue(t *testing.T) {
	e := newToolEnv(t)
	ctx, _ := e.turn("sess", false)
	_, err := e.reg.ExecuteTool(ctx, styleNoteTool, map[string]interface{}{"value": "кратко"})
	require.Error(t, err)
	_, err = e.reg.ExecuteTool(ctx, styleNoteTool, map[string]interface{}{"key": "length"})
	require.Error(t, err)
	assert.Empty(t, e.pending(t))
}

// The confirmed notes reach the prompt of the next turn in their own section, and
// the rules section keeps only the rules.
func TestConfirmedStyleNotesAreInTheirOwnSectionOfThePrompt(t *testing.T) {
	e := newAttachmentEnv(t)
	o := orchestratorWith(t, e, answeringModel())
	ctx := context.Background()
	require.NoError(t, e.db.Queries.SaveGlobalRule(ctx, db.SaveGlobalRuleParams{
		UserID: e.userID, Kind: proposals.RuleKindStyle, Key: "humour", Value: "БЕЛЕЖКА-ЗА-ШЕГАТА",
	}))
	require.NoError(t, e.db.Queries.SaveGlobalRule(ctx, db.SaveGlobalRuleParams{
		UserID: e.userID, Kind: proposals.RuleKindRule, Key: "language", Value: "ПРАВИЛО-ЗА-ЕЗИКА",
	}))
	storeTurnMessages(t, e, "s-style", [2]string{"user", "здрасти"})

	prep, err := o.prepareTurn(ctx, e.userID, ChatRequest{SessionID: "s-style", Content: "здрасти"}, false)
	require.NoError(t, err)
	system := prep.messages[0].Content

	section := "### Как говорим ние двамата"
	require.Contains(t, system, section)
	assert.Contains(t, system, section+" (наблюдения от вашите разговори — така му харесва):\n- БЕЛЕЖКА-ЗА-ШЕГАТА\n")
	assert.Contains(t, system, "ПРАВИЛО-ЗА-ЕЗИКА")
	assert.Equal(t, 1, strings.Count(system, "БЕЛЕЖКА-ЗА-ШЕГАТА"), "a note is not repeated among the rules")
}

func TestNoStyleNotesNoSection(t *testing.T) {
	previous := prompts.GlobalManager
	prompts.Init("")
	t.Cleanup(func() { prompts.GlobalManager = previous })
	system, err := buildSystemPrompt(chatContext{})
	require.NoError(t, err)
	assert.NotContains(t, system.Content, "Как говорим ние двамата")
}
