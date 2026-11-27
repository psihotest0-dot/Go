-- +goose Up
-- +goose StatementBegin
-- The button of a turn ("Книги", "Интернет", "Анализ", ...; agent.ChatRequest.Mode) kept with the
-- person's message (the code review of 2026-11-27): "Регенерирай" asks again with the same button.
-- It was not kept, and the regeneration of a turn in a mode ran as a turn with none. NULL: no
-- button, and every message stored before.
ALTER TABLE messages ADD COLUMN mode TEXT;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE messages DROP COLUMN IF EXISTS mode;
-- +goose StatementEnd
