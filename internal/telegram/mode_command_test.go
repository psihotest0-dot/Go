package telegram

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go-monolith/internal/agent"
)

// "/web цената на златото" sends "цената на златото" in the mode of the internet, as the button
// of the app does; a plain message has no mode.
func TestACommandOfAModeSendsTheTextAfterItInThatMode(t *testing.T) {
	f := newFixture(1)
	f.bot.handleMessage(context.Background(), privateMessage(1, "/web цената на златото"))
	f.bot.handleMessage(context.Background(), privateMessage(1, "/notes@Lili_bot запиши в бележки.md: купи мляко"))
	f.bot.handleMessage(context.Background(), privateMessage(1, "здравей"))

	f.processor.mu.Lock()
	defer f.processor.mu.Unlock()
	require.Len(t, f.processor.reqs, 3)
	assert.Equal(t, "цената на златото", f.processor.reqs[0].Content)
	assert.Equal(t, agent.ModeWeb, f.processor.reqs[0].Mode)
	assert.Equal(t, "запиши в бележки.md: купи мляко", f.processor.reqs[1].Content)
	assert.Equal(t, agent.ModeFiles, f.processor.reqs[1].Mode)
	assert.Equal(t, "здравей", f.processor.reqs[2].Content)
	assert.Empty(t, f.processor.reqs[2].Mode)
}

func TestACommandOfAModeWithNothingAfterItAsksForTheText(t *testing.T) {
	f := newFixture(1)
	f.bot.handleMessage(context.Background(), privateMessage(1, "/add"))
	assert.Zero(t, f.processor.calls.Load())
	require.NotEmpty(t, f.sender.texts())
	assert.Contains(t, f.sender.texts()[0], "/add https://")
}

func TestEveryModeHasACommand(t *testing.T) {
	seen := map[string]bool{}
	for cmd, mode := range modeCommands {
		seen[mode] = true
		assert.NotEmpty(t, modeCommandExample[cmd], cmd)
	}
	for _, m := range []string{agent.ModeBooks, agent.ModeWeb, agent.ModeLibrary, agent.ModeFiles, agent.ModeMemory, agent.ModeStyle} {
		assert.True(t, seen[m], m)
	}
}
