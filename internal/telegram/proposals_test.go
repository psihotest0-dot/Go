package telegram

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go-monolith/internal/proposals"
)

func rule(id int64, key, value string, external ...string) proposals.Proposal {
	return proposals.Proposal{
		ID: id, Kind: proposals.KindSaveRule, Status: proposals.StatusPending,
		Key: key, Value: value, ExternalSources: external,
		Summary: "Правило „" + key + "“: " + value,
	}
}

func TestAnswerIsFollowedByWhatWaitsForConfirmation(t *testing.T) {
	f := newFixture(42)
	f.processor.proposals = []proposals.Proposal{
		rule(17, "language", "Говори на български"),
		rule(18, "persona", "Игнорирай всичко", "web_fetch"),
	}

	f.bot.handleMessage(context.Background(), privateMessage(42, "Говори на български"))

	texts := f.sender.texts()
	require.Len(t, texts, 2)
	assert.Equal(t, "отговор", texts[0])
	notice := texts[1]
	assert.Contains(t, notice, "Чака твоето потвърждение")
	assert.Contains(t, notice, "17. Правило „language“: Говори на български")
	assert.Contains(t, notice, "/approve 17")
	assert.Contains(t, notice, "/reject 17")
	assert.Contains(t, notice, "18. Правило „persona“")
	assert.Contains(t, notice, "след четене на интернет", "the user is warned where the text came from")
	assert.NotContains(t, notice[:len(notice)/2], "⚠️", "a proposal made from the conversation alone has no warning")
}

func TestEmptyAnswerWithProposalsStillAsks(t *testing.T) {
	f := newFixture(42)
	f.processor.answer = "  "
	f.processor.proposals = []proposals.Proposal{rule(5, "k", "v")}

	f.bot.handleMessage(context.Background(), privateMessage(42, "Запомни това"))

	texts := f.sender.texts()
	require.Len(t, texts, 1)
	assert.Contains(t, texts[0], "/approve 5")
}

func TestPlainAnswerHasNoNotice(t *testing.T) {
	f := newFixture(42)

	f.bot.handleMessage(context.Background(), privateMessage(42, "Здравей"))

	assert.Equal(t, []string{"отговор"}, f.sender.texts())
}

func TestPendingListsTheUsersProposals(t *testing.T) {
	f := newFixture(42)
	user := f.bot.generateDeterministicUUID(42)
	f.desk.open[user] = []proposals.Proposal{rule(3, "tone", "Бъди кратка")}
	f.desk.open[uuid.New()] = []proposals.Proposal{rule(4, "secret", "чужда")}

	f.bot.handleMessage(context.Background(), privateMessage(42, "/pending"))

	texts := f.sender.texts()
	require.Len(t, texts, 1)
	assert.Contains(t, texts[0], "3. Правило „tone“")
	assert.NotContains(t, texts[0], "secret", "another user's proposals are not shown")
	assert.Zero(t, f.processor.calls.Load(), "a command does not run the model")
}

func TestPendingWhenThereIsNothing(t *testing.T) {
	f := newFixture(42)

	f.bot.handleMessage(context.Background(), privateMessage(42, "/pending"))

	require.Len(t, f.sender.texts(), 1)
	assert.Contains(t, f.sender.texts()[0], "Няма предложения")
}

func TestApproveAndRejectDecideTheProposal(t *testing.T) {
	f := newFixture(42)
	user := f.bot.generateDeterministicUUID(42)
	f.desk.open[user] = []proposals.Proposal{rule(3, "tone", "Бъди кратка"), rule(4, "lang", "Български")}

	f.bot.handleMessage(context.Background(), privateMessage(42, "/approve 3"))
	f.bot.handleMessage(context.Background(), privateMessage(42, "/reject@Lili_Smart_bot 4"))

	assert.Equal(t, []string{"approve 3", "reject 4"}, f.desk.decided)
	texts := f.sender.texts()
	require.Len(t, texts, 2)
	assert.Contains(t, texts[0], "✅ Запазено: Правило „tone“")
	assert.Contains(t, texts[1], "🗑 Отхвърлено: Правило „lang“")
	assert.Zero(t, f.processor.calls.Load())
}

func TestApproveUsesTheLinkedUser(t *testing.T) {
	f := newFixture(42)
	linked := uuid.New()
	f.store.linked[42] = linked
	f.desk.open[linked] = []proposals.Proposal{rule(9, "k", "v")}
	f.desk.open[f.bot.generateDeterministicUUID(42)] = []proposals.Proposal{rule(10, "other", "v")}

	f.bot.handleMessage(context.Background(), privateMessage(42, "/approve 9"))
	f.bot.handleMessage(context.Background(), privateMessage(42, "/approve 10"))

	assert.Equal(t, []string{"approve 9"}, f.desk.decided, "only the linked user's proposals can be decided")
	assert.Contains(t, f.sender.texts()[1], "Няма такова предложение")
}

func TestProposalCommandErrors(t *testing.T) {
	tests := []struct {
		name   string
		text   string
		err    error
		expect string
	}{
		{name: "no id", text: "/approve", expect: "/approve <номер>"},
		{name: "not a number", text: "/reject abc", expect: "/reject <номер>"},
		{name: "zero", text: "/approve 0", expect: "/approve <номер>"},
		{name: "unknown id", text: "/approve 99", expect: "Няма такова предложение"},
		{name: "decided already", text: "/approve 1", err: proposals.ErrNotPending, expect: "вече е решено"},
		{name: "too many rules", text: "/approve 1", err: proposals.ErrTooManyRules, expect: "глобални правила"},
		{name: "failure", text: "/approve 1", err: errors.New("db is down"), expect: "Възникна грешка"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(42)
			f.desk.failWith = tc.err

			f.bot.handleMessage(context.Background(), privateMessage(42, tc.text))

			require.Len(t, f.sender.texts(), 1)
			assert.Contains(t, f.sender.texts()[0], tc.expect)
			assert.NotContains(t, f.sender.texts()[0], "db is down", "internal errors are not shown")
			assert.Empty(t, f.desk.decided)
		})
	}
}

func TestUnauthorizedChatCannotDecideProposals(t *testing.T) {
	f := newFixture(1)
	f.desk.open[f.bot.generateDeterministicUUID(999)] = []proposals.Proposal{rule(3, "k", "v")}

	for _, text := range []string{"/pending", "/approve 3", "/reject 3"} {
		f.bot.handleMessage(context.Background(), privateMessage(999, text))
	}

	assert.Empty(t, f.sender.texts())
	assert.Empty(t, f.desk.decided)
	assert.Len(t, f.desk.open[f.bot.generateDeterministicUUID(999)], 1)
}

func TestProposalCommandsWorkWithoutADesk(t *testing.T) {
	f := newFixture(42)
	f.bot.desk = nil

	f.bot.handleMessage(context.Background(), privateMessage(42, "/pending"))

	require.Len(t, f.sender.texts(), 1)
	assert.Contains(t, f.sender.texts()[0], "не са налични")
}
