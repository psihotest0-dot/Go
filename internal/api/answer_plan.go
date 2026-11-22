package api

import (
	"encoding/json"

	"github.com/sqlc-dev/pqtype"
)

// RememberedMemories are the memories a planned answer used (П55, step 2.5): the start of each
// memory of the plan (messages.answer_plan) whose mark the answer had, in the order of the plan.
// A memory the model was given but did not mark is left out: the line under the answer says what
// Lili remembered, not what she was offered. nil for an answer without a plan or without marks.
func RememberedMemories(plan pqtype.NullRawMessage) []string {
	if !plan.Valid {
		return nil
	}
	var stored struct {
		Memories []struct {
			Label string `json:"label"`
		} `json:"memories"`
		Marks []int `json:"marks"`
	}
	if json.Unmarshal(plan.RawMessage, &stored) != nil {
		return nil
	}
	marked := map[int]bool{}
	for _, n := range stored.Marks {
		marked[n] = true
	}
	var out []string
	for i, m := range stored.Memories {
		if marked[i+1] && m.Label != "" {
			out = append(out, m.Label)
		}
	}
	return out
}
