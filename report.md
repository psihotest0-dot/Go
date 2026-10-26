## Diagnostic Report on Background LLM Tasks

Based on an analysis of the background workers (`internal/jobs/jobs.go`, `internal/jobs/distillation.go`, `internal/agent/orchestrator_history.go`, and the LLM client `internal/llm/client.go`), here are the findings:

### 1. Trigger Frequency

*   **Memory Consolidation**:
    *   Enqueued periodically every 5 minutes by the `MemoryConsolidationPeriodicWorker`. This job queues a `MemoryConsolidationWorker` for *each user* that has unanalyzed messages in the database.
    *   When the `MemoryConsolidationWorker` runs, it processes up to 5 unanalyzed messages at a time. It uses an advisory lock to prevent concurrent processing for the same user.
    *   It is *not* triggered immediately after every user message; it happens on the 5-minute schedule.
*   **Rolling Summary**:
    *   Triggered dynamically during a chat session inside the `Orchestrator.rollingSummary` method (which runs in the hot path of handling messages).
    *   A new summary generation is spun off in a goroutine (`go func(...)`) whenever the number of dropped (pruned) messages in the chat history exceeds the `lastDropCount` by at least 6 messages.
*   **Document Distillation**:
    *   Triggered asynchronously via River queue (typically after file ingestion) by the `DistillationWorker`.
    *   It processes files in batches of 5 chunks at a time.

### 2. LLM Request Parameters

*   **Memory Consolidation (`MemoryConsolidationWorker.analyzeMessage`)**:
    *   Uses `ChatWithJSONSchema`.
    *   **MaxTokens/Temperature limits**: None are explicitly set in the request object. It relies on the provider defaults.
    *   **Response Format**: `ChatWithJSONSchema` internally uses `openai.ChatCompletionResponseFormatTypeJSONSchema` with `Strict: true`. It successfully builds the schema dynamically from the `[]Fact{}` struct.
*   **Rolling Summary (`Orchestrator.rollingSummary`)**:
    *   Uses `Chat` (standard completion).
    *   **MaxTokens/Temperature limits**: None are explicitly set.
    *   **Response Format**: None explicitly set; it expects standard text.
*   **Document Distillation (`DistillationWorker.Work`)**:
    *   Uses `ChatWithJSONSchema`.
    *   **MaxTokens/Temperature limits**: None are explicitly set.
    *   **Response Format**: Uses JSON schema based on the `DistillationSchema{}` struct.
*   **LLM Client Config**:
    *   In `internal/llm/client.go`, the configuration explicitly bypasses `EmptyMessagesLimit` with `4294967295`.

### 3. Prompt Structure & Vulnerability

*   **Memory Reflector Prompt (`prompts/memory_reflector.tmpl`)**:
    *   The prompt instructs the model to extract facts about the user and return an array of JSON objects.
    *   Vulnerability: While it uses structured output (JSON Schema), the prompt itself lacks robust instructions on handling cases where *no facts* are found. Since there are no limits on tokens, a confused model could attempt to fill the JSON response with verbose, imagined statements if it doesn't clearly understand how to return an empty array `[]`.
*   **Rolling Summary Prompt (`prompts/rolling_summary.tmpl`)**:
    *   Instructs the model to make a short summary of the conversation.
    *   Vulnerability: Open-ended structure. Without a `MaxTokens` limit, if the conversation is highly complex or confusing, the model might try to generate an extremely long summary instead of a concise one, causing massive token outputs (e.g., 2600+ tokens).
*   **Distillation Prompt**:
    *   Hardcoded system prompt: "You are an expert at extracting core concepts, themes, and summaries from text. Extract the main thesis, themes, and write a high-level summary of the provided text."
    *   Vulnerability: Since `Summary` is a string field in the JSON Schema without length bounds in the prompt or token constraints, the model could easily write a summary that is multiple pages long.

### 4. Error Handling & Retries

*   **Retry Logic**: The `LemonadeClient` (`internal/llm/client.go`) implements a retry loop. It retries up to 3 times (4 total attempts) on *any* error from the LLM provider, with exponential backoff (100ms, 200ms, 400ms).
*   **Memory Consolidation Error Handling**:
    *   If `analyzeMessage` fails (e.g., JSON unmarshal fails because the model returned rambling text instead of valid JSON), it logs the error, skips the message, and continues processing the batch.
    *   Crucially, if an error happens during `analyzeMessage`, it does *not* mark the message as analyzed (unless the message was empty/not a user message). Because of this, the message remains "unanalyzed" in the database.
    *   The River worker will complete (or fail depending on timeouts), and the periodic scheduler will trigger it again 5 minutes later.
    *   **Infinite Loop Risk**: Yes. Since a failed extraction leaves the message unanalyzed, the next periodic run will pick up the *exact same message* and try to extract facts again. If the model consistently struggles with that specific message (e.g., generating massive text that times out or fails JSON parsing), it will become an infinite loop of heavy LLM calls every 5 minutes.
*   **Rolling Summary Error Handling**:
    *   If generation fails, it logs the error and the goroutine exits. It does not update the `lastDropCount`. Therefore, the next time a message is sent in the chat, it will likely trigger the summary generation again for the same block of old messages.
*   **Distillation Error Handling**:
    *   If a batch fails (JSON parsing fails), it logs the error, increments `failCount`, and continues to the next batch.
    *   If *all* batches fail, the worker returns an error, which causes River to retry the entire job. This also has the potential to loop heavily if a document consistently confuses the model.

### Summary of Resource Leak Causes

1.  **Missing Constraints**: None of the background LLM tasks set `MaxTokens` or `Temperature`. They are prone to runaway open-ended generation by default.
2.  **Infinite Retries on Bad Data**: The combination of `LemonadeClient` retries (4 attempts) and the job schedulers failing to mark problematic inputs as "processed" (or permanently failed) creates infinite loops. A single weird user message can trigger 4 heavy LLM calls every 5 minutes forever.
3.  **JSON Schema Fallback Risks**: While JSON schema validation is strict, the models might get stuck trying to generate valid JSON if the prompt doesn't explicitly guide them on how to handle edge cases (e.g. "return empty array if no facts").
