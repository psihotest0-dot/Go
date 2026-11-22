-- +goose Up
-- +goose StatementBegin
-- The plan of an answer to a personal message (П55 phase 1, 2026-11-22): which of the person's
-- memories the code gave the model for the reply, and which of them the reply marked as used.
--
-- The code decides what a reply to "уморен съм" is made of (internal/answerplan) and the model
-- only writes it. The column is what the next turns need: a plan comes at most once in five
-- replies of a conversation (the last replies are read for that), a memory of the person's last
-- few plans is not used again while there are others, and the clients can show the line "Лили си
-- спомни: ..." under the answer.
--
-- {"memories": [{"id": "<uuid>", "label": "<the first words>"}], "mode": "thought",
--  "marks": [1, 2], "unknown_marks": 0, "score": 0.052}
--
-- NULL: an answer without a plan (every answer stored before this column too), or a message of
-- the person. Nullable and without a default, so the column is added without rewriting the table.
ALTER TABLE messages ADD COLUMN answer_plan JSONB;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE messages DROP COLUMN answer_plan;
-- +goose StatementEnd
