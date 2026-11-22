package api

import (
	"testing"

	"github.com/sqlc-dev/pqtype"
	"github.com/stretchr/testify/assert"

	"go-monolith/internal/db"
)

func plan(s string) pqtype.NullRawMessage {
	return pqtype.NullRawMessage{RawMessage: []byte(s), Valid: true}
}

func TestRememberedMemoriesAreTheMarkedOnes(t *testing.T) {
	stored := plan(`{"memories":[{"label":"диванът"},{"label":"смените"},{"label":"дъщерята"}],"marks":[3,1],"unknown_marks":1}`)
	assert.Equal(t, []string{"диванът", "дъщерята"}, RememberedMemories(stored), "in the order of the plan, only the marked")
	assert.Nil(t, RememberedMemories(plan(`{"memories":[{"label":"диванът"}],"marks":[]}`)), "offered but not used")
	assert.Nil(t, RememberedMemories(plan(`{"memories":[{"label":"диванът"}],"marks":[2]}`)), "a mark of no memory")
	assert.Nil(t, RememberedMemories(pqtype.NullRawMessage{}))
	assert.Nil(t, RememberedMemories(plan(`not json`)))
}

// GET /messages carries them on the planned answer only.
func TestAMessageOfAPlanCarriesTheRememberedMemories(t *testing.T) {
	planned := toMessageDTO(db.Message{Role: "assistant", AnswerPlan: plan(`{"memories":[{"label":"диванът"}],"marks":[1]}`)})
	assert.Equal(t, []string{"диванът"}, planned.Remembered)
	assert.Nil(t, toMessageDTO(db.Message{Role: "assistant"}).Remembered)
}
