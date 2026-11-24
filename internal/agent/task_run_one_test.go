package agent

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go-monolith/internal/taskrun"
)

func TestWhichMessagesAskForOneWholeDocument(t *testing.T) {
	yes := []string{
		"прочети цялата статия и запиши подробни бележки във файл",
		"извлечи всичко от документа подробно в един файл",
		"прочети изцяло книгата и ми запиши бележки по нея във файл",
		"read the whole paper and save detailed notes to a file",
		"запиши подробни бележки по статията във файл", // detailed notes need the whole text
	}
	no := []string{
		"Запиши цялата статия във файл: https://arxiv.org/abs/1706.03762",            // the page word for word
		"прочети цялата статия и запиши подробни бележки: https://example.org/a.pdf", // a link: still to be added
		"прочети цялата статия и ми кажи за какво е",                                 // no file, no notes
		"запиши кратки бележки по статията във файл",                                 // not the whole
		"прочети целия файл бележки.md",                                              // no document
	}
	for _, m := range yes {
		assert.True(t, wholeOneDocument(m), m)
		assert.Empty(t, taskRunKind(m), "one document is not a task over many: %s", m)
	}
	for _, m := range no {
		assert.False(t, wholeOneDocument(m), m)
	}
}

// One document: the one added last in the conversation, whatever else was added before it.
func TestOneWholeDocumentIsTheNewestAdded(t *testing.T) {
	e := newTaskRunEnv(t)
	e.addDocument(t, "old-book.pdf", "2 days")
	storeTurnMessages(t, e.attachmentEnv, "tr", [2]string{"user", "добави ги"}, [2]string{"assistant", "Добавих ги."})
	e.addDocument(t, "paper-a.pdf", "")
	e.addDocument(t, "paper-b.pdf", "")

	out := e.say(t, "tr", "прочети цялата статия и запиши подробни бележки във файл")

	assert.Contains(t, out, "за документа, добавен в библиотеката")
	assert.Contains(t, out, "• paper-b.pdf")
	assert.NotContains(t, out, "paper-a.pdf")
	assert.NotContains(t, out, "old-book.pdf")
	assert.Empty(t, e.model.Requests, "the model was not asked")
	proposed, err := e.plans.Proposed(context.Background(), e.userID, "tr")
	require.NoError(t, err)
	require.NotNil(t, proposed)
	assert.Equal(t, taskrun.KindFull, proposed.Kind)
	require.Len(t, proposed.Steps, 1)
	assert.Equal(t, "paper-b.pdf", proposed.Steps[0].Text)
	assert.Equal(t, "подробни бележки по целия текст на документа", proposed.Goal)
}

func TestOneWholeDocumentWithNothingAddedIsAnsweredByTheCode(t *testing.T) {
	e := newTaskRunEnv(t)
	e.addDocument(t, "old-book.pdf", "2 days")
	storeTurnMessages(t, e.attachmentEnv, "tr", [2]string{"user", "здрасти"}, [2]string{"assistant", "Здравей!"})

	out := e.say(t, "tr", "прочети цялата статия и запиши подробни бележки във файл")

	assert.Contains(t, out, "не е добавен нито един документ")
	assert.Empty(t, e.model.Requests)
}
