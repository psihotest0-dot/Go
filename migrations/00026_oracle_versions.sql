-- +goose Up
-- +goose StatementBegin
-- Two labels that let a row of the Oracle's journal be told apart from its neighbours
-- without reading the request body.
--
-- signal_version: the version of the strategy that made the signal, as the bot sent
-- it (request -> signal -> version). It is generated from the stored request, so it
-- cannot disagree with it and the rows already in the table get it too. The bot's
-- test questions carry the version 'oracle-probe'; the signals of the strategy carry
-- its own version (1.0.0 at the time of writing). NULL when the body has none.
--
-- prompt_version: which question the model was asked (the hash that
-- internal/oracle/promptversion.go builds from the system prompt, the answer schema,
-- the call parameters and the layout of the facts). The bot's evaluation says that a
-- changed prompt starts a new period, and without this label the periods could only
-- be guessed from dates. '' means "not recorded": the rows written before this
-- migration, and a call that failed before the prompt was rendered.
ALTER TABLE oracle_evaluations
    ADD COLUMN signal_version TEXT GENERATED ALWAYS AS (request -> 'signal' ->> 'version') STORED,
    ADD COLUMN prompt_version TEXT NOT NULL DEFAULT ''
        CONSTRAINT oracle_evaluations_prompt_version_shape
        CHECK (prompt_version = '' OR prompt_version ~ '^[0-9a-f]{12}$');
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE oracle_evaluations
    DROP COLUMN IF EXISTS prompt_version,
    DROP COLUMN IF EXISTS signal_version;
-- +goose StatementEnd
