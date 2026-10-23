1. **Fix cache error handling in `internal/llm/embedder.go`**
   - In `HTTPEmbedder.EmbedTexts`, modify the `if cacheErr != nil` block around `CreateEmbeddingCache`.
   - Use `*pq.Error` type assertion to check if `err.Code == "23505"`.
   - Ignore if it's a unique constraint violation, otherwise log a warning with `slog.Warn`.

2. **Fix context manager message pruning in `internal/llm/context_manager.go`**
   - Modify `pruneHistory` to count backwards from the end.
   - For every message included, keep checking if there are related messages we must include (e.g. keeping `tool` messages together with their parent `assistant` message containing the `tool_calls`).
   - If we encounter a `tool` message, include all `tool` messages for the same `tool_calls` and the parent `assistant` message. Ensure that we don't break OpenAI API requirements. (Actually OpenAI requires that if we include `tool` responses, we must include the `assistant` message with `tool_calls`).

3. **Remove chunking and mean pooling from `internal/llm/embedder.go`**
   - Remove `chunkText` function from `internal/llm/embedder.go`.
   - Modify `HTTPEmbedder.EmbedTexts` to just embed the `texts` array directly (or truncate each text if we want, but the requirement says "нека той просто векторизира подадения му стринг, или ако стрингът е над лимита, да го отрязва/truncate", actually we can just pass it directly to the embedder assuming the embedder handles truncation, or we can truncate to some reasonable rune length). I'll just pass the texts directly. Wait, the Python embedder `paraphrase-multilingual-MiniLM-L12-v2` truncates to 512 tokens automatically, but let's check if there's any other truncation needed. Passing directly is fine since the requirement says "Остави нарязването на текстовете да се случва изключително в `IngestFileWorker`...".

4. **Complete pre-commit steps to ensure proper testing, verification, review, and reflection are done.**
   - Run tests and verifications.
