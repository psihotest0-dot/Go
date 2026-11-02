package prompts

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEveryEmbeddedTemplateLoads(t *testing.T) {
	names := TemplateNames()
	require.NotEmpty(t, names)
	for _, want := range []string{"chat_system.tmpl", "memory_reflector.tmpl", "rolling_summary.tmpl", "rolling_summary_input.tmpl"} {
		assert.Contains(t, names, want)
	}

	// An empty override directory means the embedded copies only, whatever the
	// working directory holds.
	t.Chdir(t.TempDir())
	manager := NewTemplateManager("")
	require.NoError(t, manager.Preload(names...))
}

// The prompt is the same whatever the line endings of the checkout were: on
// Windows git turns the templates into CRLF files and they are embedded as they
// are, which put 111 carriage returns (about 120 tokens) into the chat prompt.
func TestRenderedPromptsHaveNoCarriageReturns(t *testing.T) {
	manager := NewTemplateManager("")
	data := map[string]interface{}{
		"CurrentDate": "2026-11-02 10:00:00", "Rules": []string{"правило"}, "TopMemories": []string{"факт"},
		"RollingSummary": "обобщение", "RelevantMemories": []string{"спомен"}, "Chunks": []string{"откъс"},
		"PreviousSummary": "старо", "MessagesText": "нови",
	}
	for _, name := range TemplateNames() {
		out, err := manager.Render(name, data)
		require.NoError(t, err, name)
		assert.NotContains(t, out, "\r", name)
	}
}

func TestOverrideWithCRLFRendersWithoutCarriageReturns(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "rolling_summary.tmpl"), []byte("първи ред\r\nвтори ред\r\n"), 0o644))

	out, err := NewTemplateManager(dir).Render("rolling_summary.tmpl", nil)

	require.NoError(t, err)
	assert.Equal(t, "първи ред\nвтори ред\n", out)
}

func TestEmbeddedTemplatesRender(t *testing.T) {
	manager := NewTemplateManager("")

	chat, err := manager.Render("chat_system.tmpl", map[string]interface{}{
		"CurrentDate":      "2026-11-01 10:00:00",
		"Rules":            []string{"Говори на български"},
		"TopMemories":      []string{"Живее в София"},
		"RollingSummary":   "обобщение-X",
		"RelevantMemories": []string{"спомен-Y"},
		"Chunks":           []string{"откъс-Z"},
	})
	require.NoError(t, err)
	for _, want := range []string{"2026-11-01 10:00:00", "Говори на български", "Живее в София", "обобщение-X", "спомен-Y", "откъс-Z"} {
		assert.Contains(t, chat, want)
	}

	instructions, err := manager.Render("rolling_summary.tmpl", nil)
	require.NoError(t, err)
	assert.NotEmpty(t, instructions)

	input, err := manager.Render("rolling_summary_input.tmpl", map[string]interface{}{
		"PreviousSummary": "старо-резюме",
		"MessagesText":    "нови-съобщения",
	})
	require.NoError(t, err)
	assert.Contains(t, input, "старо-резюме")
	assert.Contains(t, input, "нови-съобщения")

	withoutPrevious, err := manager.Render("rolling_summary_input.tmpl", map[string]interface{}{
		"PreviousSummary": "",
		"MessagesText":    "нови-съобщения",
	})
	require.NoError(t, err)
	assert.NotContains(t, withoutPrevious, "Предишно обобщение")

	_, err = manager.Render("memory_reflector.tmpl", nil)
	require.NoError(t, err)
}

func TestUnknownTemplateIsAnError(t *testing.T) {
	_, err := NewTemplateManager("").Render("no_such_template.tmpl", nil)
	require.Error(t, err)
}

func TestOverrideDirectoryWinsAndFallsBack(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rolling_summary.tmpl")
	require.NoError(t, os.WriteFile(path, []byte("override-v1"), 0o644))

	manager := NewTemplateManager(dir)

	got, err := manager.Render("rolling_summary.tmpl", nil)
	require.NoError(t, err)
	assert.Equal(t, "override-v1", got)

	// A template missing from the override directory comes from the embedded set.
	embedded, err := NewTemplateManager("").Render("memory_reflector.tmpl", nil)
	require.NoError(t, err)
	got, err = manager.Render("memory_reflector.tmpl", nil)
	require.NoError(t, err)
	assert.Equal(t, embedded, got)

	// An edited file is picked up without a restart.
	require.NoError(t, os.WriteFile(path, []byte("override-v2"), 0o644))
	later := time.Now().Add(time.Minute)
	require.NoError(t, os.Chtimes(path, later, later))
	got, err = manager.Render("rolling_summary.tmpl", nil)
	require.NoError(t, err)
	assert.Equal(t, "override-v2", got)
}

func TestBrokenOverrideIsReportedAtPreload(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "chat_system.tmpl"), []byte("{{ .Unclosed "), 0o644))

	err := NewTemplateManager(dir).Preload("chat_system.tmpl")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "chat_system.tmpl")
}

func TestEmptyOverrideDoesNotReadTheWorkingDirectory(t *testing.T) {
	// A stray file with a template's name in the working directory must not
	// replace the embedded template when no override directory is configured.
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "rolling_summary.tmpl"), []byte("stray-file"), 0o644))
	t.Chdir(dir)

	got, err := NewTemplateManager("").Render("rolling_summary.tmpl", nil)
	require.NoError(t, err)
	assert.NotEqual(t, "stray-file", got)
}
