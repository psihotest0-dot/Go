package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	openai "github.com/sashabaranov/go-openai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go-monolith/internal/api"
	"go-monolith/internal/db"
)

// Every fourth reply of a conversation is asked to end with a question, the others with a
// statement (plan item P49).
func TestClosingNoteAsksForAQuestionOnEveryFourthReply(t *testing.T) {
	for replies := int64(0); replies < 12; replies++ {
		want := closingNoteStatement
		if replies%4 == 3 {
			want = closingNoteQuestion
		}
		assert.Equal(t, want, closingNoteFor(replies, "как си", false, 4), "after %d replies", replies)
	}
	// Across 100 replies exactly a quarter end with a question.
	questions := 0
	for replies := int64(0); replies < 100; replies++ {
		if closingNoteFor(replies, "как си", false, 4) == closingNoteQuestion {
			questions++
		}
	}
	assert.Equal(t, 25, questions)
}

// The distance is a setting: every second reply, or no note at all.
func TestClosingNoteFollowsTheDistanceOfTheSetting(t *testing.T) {
	assert.Equal(t, closingNoteStatement, closingNoteFor(0, "как си", false, 2))
	assert.Equal(t, closingNoteQuestion, closingNoteFor(1, "как си", false, 2))
	assert.Equal(t, closingNoteStatement, closingNoteFor(2, "как си", false, 2))
	assert.Equal(t, closingNoteQuestion, closingNoteFor(0, "как си", false, 1), "every reply")
	for replies := int64(0); replies < 8; replies++ {
		assert.Empty(t, closingNoteFor(replies, "как си", false, 0), "0 puts the notes off")
		assert.Empty(t, closingNoteFor(replies, "как си", false, -1), "a negative distance is no dose")
	}
}

// A technical answer is not shaped by the note: the prompt lets it be as long as it needs.
func TestClosingNoteStaysOutOfATechnicalTurn(t *testing.T) {
	for _, replies := range []int64{0, 3} {
		assert.Empty(t, closingNoteFor(replies, "оправи това:\n```go\nfunc f() {}\n```", false, 4), "code in the message")
		assert.Empty(t, closingNoteFor(replies, "какво пише във файла", true, 4), "a file attached to the conversation")
	}
}

// The notes ask for what the plan says they ask for: one question, about a detail, never a
// generic one; the other note asks for a statement. A change of their words is a decision to
// measure again, not an edit.
func TestClosingNotesSayWhatTheyMeanTo(t *testing.T) {
	for _, phrase := range []string{"ONE short question", "specific detail", "what else interests you", "not two questions", "right after the person answered one of yours"} {
		assert.Contains(t, closingNoteQuestion, phrase)
	}
	assert.Contains(t, closingNoteStatement, "end this reply with a statement")
	assert.Contains(t, closingNoteStatement, "not with a question")
	// They are told apart from the person's own words.
	for _, note := range []string{closingNoteQuestion, closingNoteStatement} {
		assert.Contains(t, note, "not from the user")
	}
}

// storePairs stores n question-and-answer pairs: n replies given before the turn.
func storePairs(t *testing.T, e *attachmentEnv, session string, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		storeTurnMessages(t, e, session,
			[2]string{"user", fmt.Sprintf("въпрос %d", i)}, [2]string{"assistant", fmt.Sprintf("отговор %d", i)})
	}
}

// storeReplies stores n pairs and then the question of the turn.
func storeReplies(t *testing.T, e *attachmentEnv, session string, n int, last string) {
	t.Helper()
	storePairs(t, e, session, n)
	storeTurnMessages(t, e, session, [2]string{"user", last})
}

func lastMessage(t *testing.T, prep preparedTurn) string {
	t.Helper()
	require.NotEmpty(t, prep.messages)
	return prep.messages[len(prep.messages)-1].Content
}

func TestPrepareTurnPutsTheNoteNextToTheLastMessageOnly(t *testing.T) {
	e := newAttachmentEnv(t)
	o := orchestratorWith(t, e, answeringModel())

	// Another conversation of the same person has already had three replies: it is not this one.
	storePairs(t, e, "cn-other", 3)
	storeReplies(t, e, "cn-first", 0, "здравей")
	first, err := o.prepareTurn(context.Background(), e.userID, ChatRequest{SessionID: "cn-first", Content: "здравей"}, false)
	require.NoError(t, err)
	assert.Equal(t, "здравей"+closingNoteStatement, lastMessage(t, first), "the first reply ends with a statement")

	storeReplies(t, e, "cn-fourth", 3, "разкажи ми още")
	fourth, err := o.prepareTurn(context.Background(), e.userID, ChatRequest{SessionID: "cn-fourth", Content: "разкажи ми още"}, false)
	require.NoError(t, err)
	assert.Equal(t, "разкажи ми още"+closingNoteQuestion, lastMessage(t, fourth), "the fourth reply ends with a question")
	for _, m := range fourth.messages[1 : len(fourth.messages)-1] {
		assert.NotContains(t, m.Content, "[Note for the answer", "the earlier messages of the history carry no note")
	}
}

// The count is of the conversation, not of the window the prompt is built from: the window
// holds only the newest messages, and a count made inside it would stop at the same number
// for every long conversation.
func TestPrepareTurnCountsTheWholeConversationNotTheWindow(t *testing.T) {
	e := newAttachmentEnv(t)
	o := orchestratorWith(t, e, answeringModel())
	// The test budgets keep 10 messages: 5 answers at most are in the window. 11 answers
	// before the question is the 12th reply: the question turn (11 % 4 == 3); the window alone
	// would count 5 (a statement).
	storeReplies(t, e, "cn-long", 11, "а после?")

	prep, err := o.prepareTurn(context.Background(), e.userID, ChatRequest{SessionID: "cn-long", Content: "а после?"}, false)
	require.NoError(t, err)

	assert.Less(t, len(prep.messages), 14, "the history was cut, so the window really is shorter than the conversation")
	assert.Equal(t, "а после?"+closingNoteQuestion, lastMessage(t, prep))
}

// An empty answer (the model gave nothing) is not a reply: the prompt leaves it out, and so does the count.
func TestAnEmptyAnswerIsNotCountedAsAReply(t *testing.T) {
	e := newAttachmentEnv(t)
	o := orchestratorWith(t, e, answeringModel())
	storePairs(t, e, "cn-empty", 2)
	storeTurnMessages(t, e, "cn-empty", [2]string{"user", "ехо?"}, [2]string{"assistant", ""})
	storeTurnMessages(t, e, "cn-empty", [2]string{"user", "и сега?"}, [2]string{"assistant", "ето"}, [2]string{"user", "добре"})

	prep, err := o.prepareTurn(context.Background(), e.userID, ChatRequest{SessionID: "cn-empty", Content: "добре"}, false)
	require.NoError(t, err)

	// three replies were given (the empty one does not count): the fourth asks for a question
	assert.Equal(t, "добре"+closingNoteQuestion, lastMessage(t, prep))
}

// Regenerating a reply asks for the same ending as the reply it replaces: the answer being
// replaced does not count as one given before.
func TestRegenerationOfTheFourthReplyStillAsksForAQuestion(t *testing.T) {
	e := newAttachmentEnv(t)
	o := orchestratorWith(t, e, answeringModel())
	storeReplies(t, e, "cn-regen", 3, "кажи още")
	storeTurnMessages(t, e, "cn-regen", [2]string{"assistant", "четвъртият отговор, който се сменя"})
	req := ChatRequest{SessionID: "cn-regen", Content: "кажи още"}

	fresh, err := o.prepareTurn(context.Background(), e.userID, req, false)
	require.NoError(t, err)
	regenerated, err := o.prepareTurn(context.Background(), e.userID, req, true)
	require.NoError(t, err)

	assert.Equal(t, "четвъртият отговор, който се сменя", lastMessage(t, fresh), "a new turn reads the stored history as it is")
	assert.Equal(t, "кажи още"+closingNoteQuestion, lastMessage(t, regenerated))
}

// CHAT_QUESTION_EVERY reaches the orchestrator: 0 leaves the messages as they were, 2 asks for a
// question on every second reply.
func TestTheSettingOfTheDoseReachesTheTurn(t *testing.T) {
	e := newAttachmentEnv(t)
	storeReplies(t, e, "cn-set", 1, "и после?")
	req := ChatRequest{SessionID: "cn-set", Content: "и после?"}

	t.Setenv("CHAT_QUESTION_EVERY", "0")
	off, err := orchestratorWith(t, e, answeringModel()).prepareTurn(context.Background(), e.userID, req, false)
	require.NoError(t, err)
	assert.Equal(t, "и после?", lastMessage(t, off), "0: no note")

	t.Setenv("CHAT_QUESTION_EVERY", "2")
	second, err := orchestratorWith(t, e, answeringModel()).prepareTurn(context.Background(), e.userID, req, false)
	require.NoError(t, err)
	assert.Equal(t, "и после?"+closingNoteQuestion, lastMessage(t, second), "2: the second reply asks")

	t.Setenv("CHAT_QUESTION_EVERY", "")
	byDefault, err := orchestratorWith(t, e, answeringModel()).prepareTurn(context.Background(), e.userID, req, false)
	require.NoError(t, err)
	assert.Equal(t, "и после?"+closingNoteStatement, lastMessage(t, byDefault), "not set: every 4th, so the second reply is a statement")
}

func TestPrepareTurnWithoutANoteForCodeOrAFile(t *testing.T) {
	e := newAttachmentEnv(t)
	o := orchestratorWith(t, e, answeringModel())
	code := "какво не е наред:\n```go\nx := 1\n```"
	storeReplies(t, e, "cn-code", 3, code)
	prep, err := o.prepareTurn(context.Background(), e.userID, ChatRequest{SessionID: "cn-code", Content: code}, false)
	require.NoError(t, err)
	assert.Equal(t, code, lastMessage(t, prep), "code in the message: no note, even on the fourth reply")

	storeReplies(t, e, "cn-file", 3, "прегледай файла")
	e.attach(t, e.userID, "cn-file", "notes.go", "package notes\n")
	withFile, err := o.prepareTurn(context.Background(), e.userID, ChatRequest{SessionID: "cn-file", Content: "прегледай файла"}, false)
	require.NoError(t, err)
	assert.Equal(t, "прегледай файла", lastMessage(t, withFile), "a file attached to the conversation: no note")
}

// The note is for the model only. A history that carried it would teach the model to write
// notes, and the person would read them in the chat.
func TestTheNoteIsNeverStoredAndTheModelGetsIt(t *testing.T) {
	e := newAttachmentEnv(t)
	model := answeringModel()
	o := orchestratorWith(t, e, model)
	storePairs(t, e, "cn-store", 3) // the handler stores the message of the turn itself

	body, _ := json.Marshal(ChatRequest{SessionID: "cn-store", Content: "нов ход"})
	req := httptest.NewRequest(http.MethodPost, "/chat/stream", bytes.NewReader(body))
	req = req.WithContext(context.WithValue(req.Context(), api.ParsedUserIDKey, e.userID))
	rec := httptest.NewRecorder()
	o.HandleStreamChat(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	require.NotEmpty(t, model.Requests)
	sent := model.Requests[0].Messages
	assert.Equal(t, "нов ход"+closingNoteQuestion, sent[len(sent)-1].Content, "the model is asked for a question on the fourth reply")

	stored, err := e.db.Queries.GetMessagesBySession(context.Background(), db.GetMessagesBySessionParams{UserID: e.userID, SessionID: "cn-store", Limit: 50})
	require.NoError(t, err)
	require.NotEmpty(t, stored)
	for _, m := range stored {
		assert.False(t, strings.Contains(m.Content, "[Note for the answer"), "stored %s message %q carries the note", m.Role, m.Content)
	}
	assert.Equal(t, "нов ход", userMessageNamed(stored, "нов ход"))
}

// userMessageNamed returns the stored user message with that text ("" when there is none).
func userMessageNamed(stored []db.Message, text string) string {
	for _, m := range stored {
		if m.Role == "user" && m.Content == text {
			return m.Content
		}
	}
	return ""
}

// Telegram and the regeneration use the same preparation: the dose is one rule for all.
func TestTelegramTurnGetsTheNoteToo(t *testing.T) {
	e := newAttachmentEnv(t)
	model := answeringModel()
	o := orchestratorWith(t, e, model)
	storePairs(t, e, "tg-cn", 3)

	_, err := o.ProcessChat(context.Background(), e.userID, ChatRequest{SessionID: "tg-cn", Content: "и после"})
	require.NoError(t, err)

	require.NotEmpty(t, model.Requests)
	sent := model.Requests[0].Messages
	assert.Equal(t, "и после"+closingNoteQuestion, sent[len(sent)-1].Content)
}

// The note takes room: it is counted in the history budget, so that it comes out of the
// history and not out of the room of the answer. Older messages that fit without it are
// dropped when the note makes them not fit.
func TestTheNoteCountsInTheHistoryBudget(t *testing.T) {
	e := newAttachmentEnv(t)
	o := orchestratorWith(t, e, answeringModel())
	limit := o.limits().historyTokens
	require.Greater(t, limit, 200)

	last, between := "къс въпрос", "x"
	// One older message that fills the budget exactly when the note is not counted.
	older := strings.Repeat("a", (limit-estimateTokens(last)-estimateTokens(between))*bytesPerToken)
	history := []openai.ChatCompletionMessage{
		{Role: "user", Content: older}, {Role: "assistant", Content: between}, {Role: "user", Content: last}}
	require.Len(t, pruneHistory(history, limit, 10), 3, "without the note the three messages fit exactly")
	storeTurnMessages(t, e, "cn-budget", [2]string{"user", older}, [2]string{"assistant", between}, [2]string{"user", last})
	prep, err := o.prepareTurn(context.Background(), e.userID, ChatRequest{SessionID: "cn-budget", Content: last}, false)
	require.NoError(t, err)

	for _, m := range prep.messages[1:] {
		assert.NotEqual(t, older, m.Content, "the old message no longer fits next to the note")
	}
	assert.Equal(t, last+closingNoteStatement, lastMessage(t, prep))
}

func TestWithoutClosingNoteGivesBackTheWordsOfThePerson(t *testing.T) {
	for _, note := range []string{closingNoteQuestion, closingNoteStatement} {
		assert.Equal(t, "имаш ли книгата", withoutClosingNote("имаш ли книгата"+note))
		// a note of another kind that came after ours stays
		assert.Equal(t, "изтрий файла\n\n"+removalNote, withoutClosingNote("изтрий файла"+note+"\n\n"+removalNote))
	}
	assert.Equal(t, "без бележка", withoutClosingNote("без бележка"))
	assert.Empty(t, withoutClosingNote(""))
}

// The filters that look at the message of the turn - a question to the library, a request to
// delete a file - read the person's words, not the note: the note is longer than the limit
// of the first and has the word "Note", which the folder filter looks for. Without that,
// every library question on the fourth reply would be answered from nothing.
func TestTheFiltersOfTheTurnDoNotSeeTheNote(t *testing.T) {
	e := newAttachmentEnv(t)
	o := orchestratorWith(t, e, answeringModel())
	library := []string{
		"имаш ли книгата Принципи на психологията", // short: only the length of the note could break it
		"Имаш ли файла CLBSV2?",                    // no word for a book: only the word "Note" could break it
		"Какви книги имаш в тази папка?",
	}
	for i, replies := range []int{0, 3} { // a statement turn and a question turn
		for j, q := range library {
			session := fmt.Sprintf("cn-filter-%d-%d", i, j)
			storeReplies(t, e, session, replies, q)
			prep, err := o.prepareTurn(context.Background(), e.userID, ChatRequest{SessionID: session, Content: q}, false)
			require.NoError(t, err)
			require.Contains(t, lastMessage(t, prep), "[Note for the answer", "the turn has its note (%d replies before)", replies)

			assert.Equal(t, q, lastUserContent(prep.messages))
			assert.Equal(t, listDocumentsTool, forcedFirstTool(prep.tools, lastUserContent(prep.messages)),
				"%q after %d replies is still a question to the library", q, replies)
		}
	}

	removal := "не всичко е наред,опитай да изтриеш файла ,не се притеснявай това е тест"
	for i, replies := range []int{0, 3} {
		session := fmt.Sprintf("cn-remove-%d", i)
		storeReplies(t, e, session, replies, removal)
		prep, err := o.prepareTurn(context.Background(), e.userID, ChatRequest{SessionID: session, Content: removal}, false)
		require.NoError(t, err)
		assert.True(t, asksToRemoveFile(lastUserContent(prep.messages)), "a request to delete a file is still one after %d replies", replies)
	}
}

// The route to the paid model follows the same words.
func TestARoutingQuestionOnTheFourthReplyStillGoesToTheLibraryRoute(t *testing.T) {
	e := newAttachmentEnv(t)
	o := orchestratorWith(t, e, answeringModel()).WithPaidModel(answeringModel(), "deepseek-flash", true)
	q := "имаш ли книгата Принципи на психологията"
	storeReplies(t, e, "cn-route", 3, q)
	prep, err := o.prepareTurn(context.Background(), e.userID, ChatRequest{SessionID: "cn-route", Content: q}, false)
	require.NoError(t, err)

	paid, reason := o.routeToPaid(prep.ctx, prep.messages, prep.tools)

	assert.True(t, paid)
	assert.Equal(t, "library", reason)
}
