package agent

import (
	"context"
	"slices"
	"sync"

	"go-monolith/internal/api"
	"go-monolith/internal/proposals"
)

// currentQueryKey stores the user message that started the current turn. Tool
// calls carry only the arguments the model chose to send, so a model that
// omits a required argument leaves the registry without the text the user
// actually asked about. Keeping the message in the context lets the search
// tools fall back to it instead of failing the whole turn.
type currentQueryKey struct{}

// WithCurrentQuery records the user message of the current turn in ctx.
func WithCurrentQuery(ctx context.Context, query string) context.Context {
	if query == "" {
		return ctx
	}
	return context.WithValue(ctx, currentQueryKey{}, query)
}

// currentQuery returns the user message recorded by WithCurrentQuery, or an
// empty string when the caller did not provide one.
func currentQuery(ctx context.Context) string {
	query, _ := ctx.Value(currentQueryKey{}).(string)
	return query
}

// Sources of untrusted text. They end up on a proposal so the user can see what
// the turn had read when the model asked to save something.
const (
	sourceWebSearch          = "web_search"
	sourceWebFetch           = "web_fetch"
	sourceSearchDocuments    = "search_documents"
	sourceListDocuments      = "list_documents"
	sourceRetrievedDocuments = "retrieved_documents"
)

// turnKey stores the turnState of the current chat turn.
type turnKey struct{}

// turnState is what the tools of one chat turn share: where the turn is
// happening, which untrusted text it has read, and the proposals it made.
type turnState struct {
	sessionID string

	mu        sync.Mutex
	external  []string
	proposals []proposals.Proposal
	// found are the passages the tools returned to the model (search_documents), in
	// the form of the sources panel; announced is how many of them the client was
	// told about already.
	found     []api.SourceInfo
	announced int
}

// withTurn starts the state of a chat turn. retrievedDocuments tells that text
// from the user's documents is already in the prompt.
func withTurn(ctx context.Context, sessionID string, retrievedDocuments bool) (context.Context, *turnState) {
	turn := &turnState{sessionID: sessionID}
	if retrievedDocuments {
		turn.markExternal(sourceRetrievedDocuments)
	}
	return context.WithValue(ctx, turnKey{}, turn), turn
}

// turnFrom returns the state of the current turn, or nil outside a chat turn.
func turnFrom(ctx context.Context) *turnState {
	turn, _ := ctx.Value(turnKey{}).(*turnState)
	return turn
}

// markExternal records that the turn has read text the user did not write.
func (t *turnState) markExternal(source string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if !slices.Contains(t.external, source) {
		t.external = append(t.external, source)
	}
}

// externalSources lists the untrusted sources the turn has read so far.
func (t *turnState) externalSources() []string {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return slices.Clone(t.external)
}

// session is the session of the turn ("" outside a chat turn).
func (t *turnState) session() string {
	if t == nil {
		return ""
	}
	return t.sessionID
}

// addProposal remembers a proposal made in this turn, once.
func (t *turnState) addProposal(p proposals.Proposal) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, known := range t.proposals {
		if known.ID == p.ID {
			return
		}
	}
	t.proposals = append(t.proposals, p)
}

// addSources remembers passages a tool gave the model, each chunk once.
func (t *turnState) addSources(sources []api.SourceInfo) {
	if t == nil || len(sources) == 0 {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.found = append(t.found, withoutKnownSources(t.found, sources)...)
}

// foundSources lists every passage the tools of the turn gave the model.
func (t *turnState) foundSources() []api.SourceInfo {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return slices.Clone(t.found)
}

// newSources lists the passages found since the last call, so each one is
// announced to the client once.
func (t *turnState) newSources() []api.SourceInfo {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	fresh := slices.Clone(t.found[t.announced:])
	t.announced = len(t.found)
	return fresh
}

// madeProposals lists the proposals the turn made.
func (t *turnState) madeProposals() []proposals.Proposal {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return slices.Clone(t.proposals)
}
