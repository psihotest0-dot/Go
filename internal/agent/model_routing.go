package agent

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"

	openai "github.com/sashabaranov/go-openai"

	"go-monolith/internal/llm"
	"go-monolith/metrics"
)

// Which model answers a turn of the chat (plan item П47).
//
// The local model is the default. When the paid provider is on (PAID_LLM_ENABLED, see
// llm.Paid) two kinds of turns go to the cloud:
//
//   - a question to the library - a book, a passage, what the library holds - unless
//     LIBRARY_LLM=local: forcedFirstTool already tells such a turn apart. Measured on
//     2026-11-14: the answers are as good by the owner's taste (blind, 14 questions: 6 for
//     the local model, 5 for the paid one, 3 equal) and about 2.5 times faster;
//   - any turn of a conversation the owner sent to the cloud (use_paid=true in the request,
//     the button of the client); use_paid=false keeps even a library question at home;
//   - an explanation from the library (explain.go), when EXPLAIN_LLM=deepseek; without it such a
//     turn stays at home even though it is a library question.
//
// If the paid model fails before it has done anything - no limit left for the day, no
// network, a refusal of the provider - the same turn is asked of the local model, so the
// chat does not stop because the cloud did. After a tool has run, or after a word was
// sent, it is not repeated: a second run would do the first one's writes again.
//
// Everything else stays local on purpose: the background work (memory, summaries,
// distillation of books) and anything that is not a library question unless the owner chose.

type llmChoiceKey struct{}

// WithLLMChoice records the owner's choice for this request: true - the cloud, false -
// the local model, nil - no choice (the library questions follow LIBRARY_LLM).
func WithLLMChoice(ctx context.Context, usePaid *bool) context.Context {
	if usePaid == nil {
		return ctx
	}
	return context.WithValue(ctx, llmChoiceKey{}, *usePaid)
}

// llmChoice is the choice recorded by WithLLMChoice, if there is one.
func llmChoice(ctx context.Context) (usePaid, chosen bool) {
	usePaid, chosen = ctx.Value(llmChoiceKey{}).(bool)
	return usePaid, chosen
}

type llmClientKey struct{}

// clientFor is the client that serves this request: the one the router chose, or the
// default one.
func (o *Orchestrator) clientFor(ctx context.Context) llm.LLMClient {
	if c, ok := ctx.Value(llmClientKey{}).(llm.LLMClient); ok {
		return c
	}
	return o.llmClient
}

// WithPaidModel gives the orchestrator the paid model of the chat: name is its name for
// the clients, library says whether the questions to the library go to it on their own.
func (o *Orchestrator) WithPaidModel(client llm.LLMClient, name string, library bool) *Orchestrator {
	o.paid, o.paidName, o.libraryPaid = client, name, library
	return o
}

// routeToPaid decides whether this turn goes to the paid model, and why.
func (o *Orchestrator) routeToPaid(ctx context.Context, messages []openai.ChatCompletionMessage, tools []openai.Tool) (bool, string) {
	if o.paid == nil {
		return false, ""
	}
	if usePaid, chosen := llmChoice(ctx); chosen {
		if usePaid {
			return true, "chosen"
		}
		return false, "chosen_local"
	}
	// An explanation from the library goes where EXPLAIN_LLM says, whatever LIBRARY_LLM says
	// (explain.go): the owner liked the local model's as much as the cloud's.
	if turnFrom(ctx).explaining() {
		if o.explainCloud {
			return true, "explain"
		}
		return false, "explain_local"
	}
	// A look on the internet the person asked for is forced too (lookup_guard.go), but it is not a
	// question to the library: it stays where the turn is.
	if forced := forcedFirstTool(tools, lastUserContent(messages)); o.libraryPaid && forced != "" && forced != "web_search" {
		return true, "library"
	}
	return false, "default"
}

// answeredBy is who gave the answer of a turn, for the client to show.
type answeredBy struct {
	Provider string `json:"provider"` // "local" or "deepseek"
	Model    string `json:"model,omitempty"`
}

// generateResponse runs the turn on the model the router picks (see the top of this file).
func (o *Orchestrator) generateResponse(
	ctx context.Context,
	finalMessages []openai.ChatCompletionMessage,
	tools []openai.Tool,
	onDelta deltaFunc,
) (string, []openai.ToolCall, error) {
	turn := turnFrom(ctx)
	// A question about the work that the code answers from the folder or the plan: no model at
	// all, and so none of the checks of a model's answer (status_answer.go).
	if text := turn.statusAnswerText(); text != "" {
		if onDelta != nil {
			if err := onDelta(text, ""); err != nil {
				return "", nil, err
			}
		}
		return text, nil, nil
	}
	paid, reason := o.routeToPaid(ctx, finalMessages, tools)
	if !paid {
		if o.paid != nil {
			metrics.ChatRoutes.WithLabelValues("local", reason).Inc()
			turn.setAnsweredBy(answeredBy{Provider: "local"})
		}
		return o.generateGuarded(ctx, finalMessages, tools, onDelta)
	}

	paidCtx := context.WithValue(ctx, llmClientKey{}, o.paid)
	content, executed, err := o.generateGuarded(paidCtx, finalMessages, tools, onDelta)
	if err == nil {
		metrics.ChatRoutes.WithLabelValues("paid", reason).Inc()
		turn.setAnsweredBy(answeredBy{Provider: "deepseek", Model: o.paidName})
		return content, executed, nil
	}

	// The request ended (the client left or the time ran out): nothing to retry for.
	if ctx.Err() != nil || len(executed) > 0 || strings.TrimSpace(content) != "" {
		metrics.ChatRoutes.WithLabelValues("paid_failed", reason).Inc()
		return content, executed, err
	}
	slog.WarnContext(ctx, "the paid model failed before it did anything; the local model answers this turn",
		slog.String("reason", reason), slog.Any("error", err))
	metrics.ChatRoutes.WithLabelValues("local", "fallback").Inc()
	turn.setAnsweredBy(answeredBy{Provider: "local"})
	return o.generateGuarded(ctx, finalMessages, tools, onDelta)
}

// modelEvent is the SSE frame that tells the client who answered; "" when there is nothing
// to tell (the paid model is not configured: the stream is what it always was).
func (o *Orchestrator) modelEvent(ctx context.Context) string {
	if o.paid == nil {
		return ""
	}
	who := turnFrom(ctx).answeredByWhom()
	if who.Provider == "" {
		return ""
	}
	payload, err := json.Marshal(who)
	if err != nil {
		return ""
	}
	return "event: model\ndata: " + string(payload) + "\n\n"
}
