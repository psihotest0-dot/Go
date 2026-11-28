package agent

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go-monolith/internal/taskrun"
	"go-monolith/internal/userfiles"
)

const folderAsk = "за всеки от файловете в папката си направи кратко резюме в нов отделен файл"

func TestATaskOverTheFolderIsProposedAndStarted(t *testing.T) {
	e := newTaskRunEnv(t)
	files := folderOf(t, e)
	for _, n := range []string{"стоицизъм.md", "навици.md", "навици - резюме.md"} {
		_, err := files.Create(e.userID, n, "текст на "+n)
		require.NoError(t, err)
	}
	asked := len(e.model.Requests)

	out := streamedText(t, e.say(t, "f", folderAsk))

	assert.Equal(t, asked, len(e.model.Requests), "the code answers")
	assert.Contains(t, out, "отделен файл с резюме за всеки от тези 2 файла")
	assert.Contains(t, out, "• навици.md")
	assert.Contains(t, out, "• стоицизъм.md")
	assert.Contains(t, out, "Пропускам 1, направени от предишни такива задачи")
	p, err := e.plans.Proposed(context.Background(), e.userID, "f")
	require.NoError(t, err)
	require.NotNil(t, p)
	assert.Equal(t, taskrun.KindSummary, p.Kind)
	require.Len(t, p.Steps, 2)
	assert.NotEmpty(t, p.Steps[0].Folder)

	assert.Contains(t, streamedText(t, e.say(t, "f", "да")), "Започнах")
	running, err := e.plans.Running(context.Background(), e.userID, "f")
	require.NoError(t, err)
	require.NotNil(t, running)
	assert.Equal(t, 1, e.waker.n)
}

func TestAnEmptyFolderIsSaidSo(t *testing.T) {
	e := newTaskRunEnv(t)
	folderOf(t, e)
	out := streamedText(t, e.say(t, "f", folderAsk))
	assert.Contains(t, out, "Папката ти е празна")
}

func TestOnlyATaskThatNamesTheFolderIsAboutIt(t *testing.T) {
	e := newTaskRunEnv(t)
	files := folderOf(t, e)
	_, err := files.Create(e.userID, "навици.md", "текст")
	require.NoError(t, err)
	for _, m := range []string{
		"за всеки от добавените източници направи отделен файл с резюме", // the library (task_run.go)
		"за всеки файл в папката извлечи всичко подробно",                // the reading of a whole text: not for the folder
	} {
		assert.Empty(t, e.o.folderTaskAnswer(context.Background(), e.userID, "f", m), m)
	}
	assert.NotEmpty(t, e.o.folderTaskAnswer(context.Background(), e.userID, "f", "направи файл с ключовите факти за всеки от моите файлове"))
}

// folderOf is the folder of the files of the task-run env.
func folderOf(t *testing.T, e *taskRunEnv) *userfiles.Store {
	t.Helper()
	require.NotNil(t, e.reg.files)
	return e.reg.files
}
