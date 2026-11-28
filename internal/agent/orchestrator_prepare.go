package agent

import (
	"context"
	"fmt"
	"slices"

	"github.com/google/uuid"
	openai "github.com/sashabaranov/go-openai"

	"go-monolith/internal/answerplan"
	"go-monolith/internal/api"
)

// preparedTurn is everything the model call of one chat turn needs. prepareTurn builds it
// for the three ways a turn starts (the chat stream, regenerate and Telegram), which
// differ only in what comes before (the HTTP request, the user message) and after (the
// stream, replacing the old answer, the returned answer).
type preparedTurn struct {
	// ctx carries the state of the turn (see withTurn); the model call and the tools run with it.
	ctx  context.Context
	turn *turnState
	// messages are the system prompt followed by the pruned history.
	messages []openai.ChatCompletionMessage
	tools    []openai.Tool
	// sources are the passages the prompt itself contains, for the sources the client shows.
	sources []api.SourceInfo
}

// prepareTurn builds the prompt of one turn: the files attached to the conversation, the
// context (rules, memories, concepts, history), the turn state, the pruned history, the
// rolling summary and the system prompt, with the trace steps of each stage.
//
// The user message of the turn must already be stored: the history is read back from the
// database. With regenerate the answer being replaced is left out of the prompt, because
// the model must write a new reply, not read (and copy) the one it replaces.
//
// A failure means that no model call was made; the caller reports it its own way (an HTTP
// error before the stream starts, or a returned error).
func (o *Orchestrator) prepareTurn(ctx context.Context, userID uuid.UUID, req ChatRequest, regenerate bool) (preparedTurn, error) {
	// The room for the files attached to the conversation comes out of the history and
	// the dynamic part of the prompt.
	plan := o.planAttachments(ctx, userID, req.SessionID, o.limits())
	chatCtx, memoryIDs, err := o.loadContextWithLimits(ctx, userID, req, plan.limits)
	chatCtx.Attachments = plan.files
	if err != nil {
		return preparedTurn{}, fmt.Errorf("failed to load context: %w", err)
	}

	if regenerate {
		chatCtx.History = dropTrailingAssistantMessages(chatCtx.History)
		if len(chatCtx.HistoryAt) > len(chatCtx.History) {
			chatCtx.HistoryAt = chatCtx.HistoryAt[:len(chatCtx.History)]
		}
	}

	// The dose of questions (P49): a note next to the message, in the copy the model reads.
	// It goes in before the history is pruned, so that its tokens come out of the history
	// budget, not out of the room of the answer.
	//
	// A conversation with a plan in progress has the plan next to the message too (plan_tool.go),
	// and no question to end with: it is a task, not a talk.
	chatCtx, taskPlan, planToolTokens := o.withPlanNote(ctx, userID, req.SessionID, chatCtx)
	chatCtx = o.withClosingNote(ctx, userID, req.SessionID, chatCtx, len(plan.files) > 0 || taskPlan != nil)
	// A personal message may get the plan of its answer (П55): its note takes the place of the
	// closing note, and it ends the reply the way that note said.
	var answerPlan *answerplan.Plan
	if o.answerPlanner != nil {
		chatCtx, answerPlan = o.withAnswerPlan(ctx, userID, req.SessionID, chatCtx,
			o.planSignals(ctx, req.Content, len(plan.files) > 0, taskPlan != nil))
	}

	// The tools of this turn share its state: what untrusted text it has read and
	// which proposals it made (see turnState).
	ctx, turn := withTurn(ctx, req.SessionID, len(chatCtx.Chunks) > 0)
	turn.setEarlierAnswer(chatCtx.EarlierAnswer)
	var hidden, attached []string
	for _, f := range plan.files {
		attached = append(attached, f.Name)
		if f.Inline == "" {
			hidden = append(hidden, f.Name)
		}
	}
	turn.setHiddenAttachments(hidden)
	turn.setCodeLanguage(attachedCodeLanguage(attached))
	turn.setAnswerPlan(answerPlan)
	turn.setPlanEvents(req.PlanEvents)
	// An explanation from the library (step 2.8): its search is forced, gathered by the code, and
	// answered by the model EXPLAIN_LLM names. In a mode only the books may explain.
	if req.Mode == "" || req.Mode == ModeBooks {
		turn.setExplain(o.explainSubjectFor(req.Content))
	}
	// "Успя ли?" about work on the files or a plan: the code answers from the folder and the plan
	// (the model's plan, else the one the server walks: task_run.go).
	statusPlan := taskPlan
	if running := o.runningPlan(ctx, userID, req.SessionID); running != nil {
		turn.markTaskRunning()
		if statusPlan == nil {
			statusPlan = running
		}
	}
	// In a mode the person asked for the road: none of these answers of the code (turn_mode.go).
	if req.Mode == "" {
		// A yes after a reminder or a report the server wrote while a proposal waited: the code asks
		// again which question it is for (waiting_proposals.go).
		turn.setStatusAnswer(o.yesAfterServerNotice(ctx, userID, req.SessionID, req.Content, chatCtx.History))
		if turn.statusAnswerText() == "" {
			turn.setStatusAnswer(o.statusAnswerFor(ctx, userID, req.Content, chatCtx.History, chatCtx.HistoryAt, statusPlan))
		}
		// A task over many documents, its yes and its stop: the code answers (task_run.go).
		if turn.statusAnswerText() == "" {
			turn.setStatusAnswer(o.taskRunAnswer(ctx, userID, req.SessionID, req.Content, historyStart(chatCtx.History, chatCtx.HistoryAt), chatCtx.History))
		}
		// "Какво са тези линкове и колко са големи?": the code opens them and answers (link_probe.go).
		if turn.statusAnswerText() == "" {
			turn.setStatusAnswer(o.linkProbeAnswer(ctx, userID, req.Content, chatCtx.History))
		}
		// "Какво можеш?": the code answers with what is on now, by the same road (help_answer.go).
		if turn.statusAnswerText() == "" {
			turn.setStatusAnswer(o.helpAnswerFor(req.Content))
		}
		// The yes to a bill that was read, and "платих …": the code answers (bill_answer.go).
		if turn.statusAnswerText() == "" {
			turn.setStatusAnswer(o.billAnswer(ctx, userID, req.SessionID, req.Content, chatCtx.History))
		}
		// A reminder, its yes, its correction, the list and "махни …": the code answers (reminder_answer.go).
		if turn.statusAnswerText() == "" {
			turn.setStatusAnswer(o.reminderAnswer(ctx, userID, req.SessionID, req.Content, chatCtx.History, false))
		}
	}
	if plan.hasText() {
		turn.markExternal(sourceAttachments)
	}
	// For a question about how far some work is: since when a change of a file counts (P54).
	turn.setRequestSince(requestSince(chatCtx.History, chatCtx.HistoryAt))

	o.trace(ctx, userID, req.SessionID, "search", map[string]interface{}{
		"query":      req.Content,
		"memory_ids": memoryIDs,
	})

	// The window keeps its start from turn to turn and is cut in steps (P51), so that the
	// prompt only grows in between and the model server can reuse its start.
	//
	// A conversation with a link the person wrote offers one more tool (add_to_library, plan
	// item P53); its definition is taken out of the room of the history, so that the prompt
	// does not grow past its budget. A turn without a link has the budgets it always had.
	userURLs := userURLKeys(chatCtx.History)
	tools := o.turnTools()
	if o.leanPrompt && len(chatCtx.Attachments) == 0 {
		// Without an attached file the tool that reads one is of no use (255 tokens a turn).
		tools = slices.DeleteFunc(tools, func(t openai.Tool) bool { return t.Function != nil && t.Function.Name == readAttachmentTool })
	}
	chatCtx.Lean = o.leanPrompt
	historyTokens := plan.limits.historyTokens
	if taskPlan != nil {
		// The tools of the plan are on offer from the start, in the place of their stub; the note
		// is part of the last message and is counted with the history.
		tools = replaceTool(tools, planStubTool, planToolDefinitions())
		historyTokens = max(0, historyTokens-planToolTokens)
		turn.markPlanActive()
	}
	if o.offersLibraryAdd(userURLs) {
		tools = append(tools, addToLibraryDefinition())
		historyTokens = max(0, historyTokens-libraryAddTokens)
		turn.setUserURLs(userURLs)
	}
	// The button of the message: its tools must be on offer, or the code says why not.
	if req.Mode == ModeReminder {
		// The button of the reminders has no tools: the code reads the request (reminder_answer.go).
		text := o.reminderAnswer(ctx, userID, req.SessionID, req.Content, chatCtx.History, true)
		if text == "" {
			text = modeOffText(req.Mode)
		}
		turn.setStatusAnswer(text)
	} else if req.Mode == ModeAnalyze {
		// The button of the analysis has no tool to require: the code finds the document and the
		// books of its subject and gives them to the model next to the message (analyze.go).
		shown := map[string]bool{}
		for _, f := range plan.files {
			shown[f.Name] = f.Inline != ""
		}
		note, answer := o.analysisNote(ctx, userID, req.SessionID, req.Content, shown)
		if answer != "" {
			turn.setStatusAnswer(answer)
		} else {
			turn.setMode(ModeAnalyze)
			chatCtx.History = withNoteOnLastUser(chatCtx.History, note)
		}
	} else if req.Mode != "" {
		if _, required := o.modeTools(tools, req.Mode, turn); len(required) > 0 {
			turn.setMode(req.Mode)
		} else {
			turn.setStatusAnswer(modeOffText(req.Mode))
		}
	}
	windowKey := summaryKey(userID, req.SessionID)
	prevStart, hasPrev := o.windows.get(windowKey)
	history, start := pruneHistorySteady(chatCtx.History, chatCtx.HistoryAt, prevStart, hasPrev,
		historyTokens, plan.limits.historyMessages)
	if !start.IsZero() {
		o.windows.put(windowKey, start)
	}
	chatCtx.RollingSum = o.rollingSummary(ctx, userID, req.SessionID, chatCtx.windowStart(len(history)))

	// The memories and the summaries of the turn after the history, for the prompt cache
	// (turn_context.go). The pruned history ends with the person's message as the full one does.
	var turnContext *openai.ChatCompletionMessage
	if o.contextAfterHistory {
		if turnContext, err = takeTurnContext(&chatCtx, history); err != nil {
			return preparedTurn{}, fmt.Errorf("failed to render the context of the turn: %w", err)
		}
	}

	systemMessage, err := buildSystemPrompt(chatCtx)
	if err != nil {
		return preparedTurn{}, fmt.Errorf("failed to render system prompt: %w", err)
	}

	messages := make([]openai.ChatCompletionMessage, 0, len(history)+2)
	messages = append(messages, systemMessage)
	messages = append(messages, history...)
	messages = withTurnContext(messages, turnContext)

	o.trace(ctx, userID, req.SessionID, "prune", map[string]interface{}{
		"total_messages": len(chatCtx.History) + 1,
		"pruned_to":      len(messages),
	})

	return preparedTurn{ctx: ctx, turn: turn, messages: messages, tools: tools, sources: chatCtx.Sources}, nil
}
