package taskrun

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go-monolith/internal/taskplan"
)

// startFolder proposes a plan over files of the folder and starts it, as the yes of the person does.
func (e *env) startFolder(t *testing.T, kind, session string, names ...string) *taskplan.Plan {
	t.Helper()
	var steps []taskplan.Step
	for i, n := range names {
		steps = append(steps, taskplan.Step{ID: i + 1, Text: n, Folder: n, Status: taskplan.StepPending})
	}
	proposed, err := e.plans.Propose(e.ctx, e.user, session, kind, "резюме на всеки файл в папката", steps)
	require.NoError(t, err)
	started, err := e.plans.Start(e.ctx, e.user, proposed.ID)
	require.NoError(t, err)
	return started
}

func TestEachFileOfTheFolderGetsAFileOfItsOwn(t *testing.T) {
	e := newEnv(t, factsAnswer)
	_, err := e.files.Create(e.user, "доходност.md", "Нощната доходност е 1.41% годишно, t-статистиката е 1.52.\nОборотът е 1.63%, а разходите 1.74%.")
	require.NoError(t, err)
	names := []string{"доходност.md", "няма-го.md"}
	e.startFolder(t, KindKeyFacts, "s", names...)

	e.walk(t)

	p := e.plan(t, "s")
	assert.Equal(t, taskplan.PlanDone, p.Status)
	assert.Equal(t, taskplan.StepDone, p.Steps[0].Status, p.Steps[0].Note)
	assert.Contains(t, p.Steps[0].Evidence, "доходност - ключови факти.md")
	text := e.read(t, "доходност - ключови факти.md")
	assert.Contains(t, text, "Източник: доходност.md (от папката)")
	assert.Contains(t, text, "- Нощната доходност е 1.41% годишно.")
	assert.Equal(t, taskplan.StepFailed, p.Steps[1].Status)
	assert.Equal(t, "файлът вече го няма в папката", p.Steps[1].Note)

	// The model got the text of the file and nothing else.
	require.NotEmpty(t, e.llm.requests)
	assert.Contains(t, e.llm.requests[0].Messages[1].Content, "t-статистиката е 1.52")
	assert.Empty(t, e.llm.requests[0].Tools)
}

func TestAShortFileOfTheFolderNeedsOneCheckedLine(t *testing.T) {
	e := newEnv(t, "- Стоицизмът учи, че ни тревожат мненията ни, не събитията.")
	_, err := e.files.Create(e.user, "стоицизъм.md", "Стоицизмът учи, че не събитията ни тревожат, а мненията ни за тях.")
	require.NoError(t, err)
	e.startFolder(t, KindSummary, "s", "стоицизъм.md")

	e.walk(t)

	p := e.plan(t, "s")
	assert.Equal(t, taskplan.StepDone, p.Steps[0].Status, p.Steps[0].Note)
	assert.FileExists(t, e.dir+"/стоицизъм - резюме.md")
}

func TestTheFilesATaskMadeAreKnown(t *testing.T) {
	for _, n := range []string{"навици - резюме.md", "доходност - ключови факти.md", "статия - подробни бележки.md"} {
		assert.True(t, MadeFile(n), n)
	}
	for _, n := range []string{"навици.md", "резюме.md", "бюджет - декември.md"} {
		assert.False(t, MadeFile(n), n)
	}
}
