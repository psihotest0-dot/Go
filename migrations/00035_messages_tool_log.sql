-- +goose Up
-- +goose StatementBegin
-- What the tools did in the turn of an answer, written by the server (2026-11-21).
--
-- The history of a conversation kept the words of the answers and not what their tools did, so the
-- model read in it "I saved the file" or an "extraction" of a paper and could not tell an answer
-- that did the work from one that only said so; a "continue" or "did you manage?" was then answered
-- from the model's own earlier words. tool_log is one short line per call of the turn ("write_file
-- notes.txt: saved", "web_fetch example.org/a: failed"), written by the server from the results of
-- the tools, and shown to the model next to its answer in the history. It is not shown to the
-- person: the clients read content.
--
-- NULL: an answer stored before this column, or a message of the person; '' is an answer whose
-- turn called no tool. Nullable and without a default, so the column is added without rewriting
-- the table.
ALTER TABLE messages ADD COLUMN tool_log TEXT;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE messages DROP COLUMN tool_log;
-- +goose StatementEnd
