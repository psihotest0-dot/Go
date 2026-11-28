package agent

import (
	"context"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/sqlc-dev/pqtype"

	"go-monolith/internal/answerplan"
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
	// sourceAttachments: the text of a file the user attached to the conversation.
	sourceAttachments = "attachments"
	// sourceUserFiles: the text of a file in the user's folder of notes (read_file). It may
	// have been saved from a web page or a book, so it is not the user's own words.
	sourceUserFiles = "user_files"
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
	// fileWrites counts the times a file tool wrote or added text in this turn (a write
	// that waits for the user is not one).
	fileWrites int
	// by is the model that gave the answer, when the paid model is configured (see model_routing.go).
	by answeredBy
	// plan is the plan of the answer to a personal message, nil for most turns (answer_plan.go).
	plan *answerplan.Plan
	// storedPlan is the plan as planReply stored it (with the marks of the answer), and
	// planEvents says that the client asked for the frame "event: plan" (ChatRequest.PlanEvents).
	storedPlan pqtype.NullRawMessage
	planEvents bool
	// userURLs are the addresses the person wrote in this conversation, by their key
	// (urlKey): the only ones add_to_library may fetch.
	userURLs map[string]string
	// libraryRows are the links add_to_library was asked for in this turn, with what became of
	// each: the person is told which were not added (libraryReport).
	libraryRows []addRow
	// archivedCopies are the pages web_fetch and save_page read from their copy in the Wayback
	// Machine in this turn: the person is told so under the answer (archiveReport, wayback.go).
	archivedCopies []archivedCopy
	// hiddenAttachments are the attached files the prompt of the turn does not show in full: a
	// message about them reads them first (attachment_force.go).
	hiddenAttachments []string
	// codeLanguage is the language of the code attached to the conversation: a search about
	// programming that names no language gets it (code_language.go).
	codeLanguage string
	// booksAndWeb: the turn searches the books and then the internet, and answers from both
	// (books_web.go).
	booksAndWeb bool
	// since is the time from which a change of a file counts as work done for a question about
	// how far the work is (requestSince); the zero time: not known.
	since time.Time
	// planActive: the conversation has a plan in progress in this turn (see plan_tool.go).
	planActive bool
	// toolLog is what the tools of the turn did, one line a call, written by the server and
	// stored with the answer (see tool_log.go).
	toolLog []string
	// calls are the calls of tools that worked in this run of the rounds, by callKey: the same
	// call again is not run (see tool_repeat.go).
	calls map[string]struct{}
	// earlier is the last answer of the conversation that rested on passages of the library
	// (earlier_answer.go).
	earlier earlierAnswer
	// explain is the subject of an explanation from the library, "" for most turns; explainDone:
	// the dossier was given (explain.go).
	explain     string
	explainDone bool
	// statusText is the answer of the code to a question about the work, "" for most turns
	// (status_answer.go): the model is not asked.
	statusText string
	// taskRunning: the server walks a task of this conversation in the background (task_run.go).
	taskRunning bool
	// mode is the button of the message (turn_mode.go), "" for none.
	mode string
}

// markPlanActive records that this turn works on a plan in progress.
func (t *turnState) markPlanActive() {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.planActive = true
}

// extraRounds are the rounds for tools this turn gets more than the usual: those of a plan.
func (t *turnState) extraRounds() int {
	if t == nil {
		return 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.planActive {
		return planActiveRounds
	}
	return 0
}

// setRequestSince records the time for statusClaimUnbacked.
func (t *turnState) setRequestSince(at time.Time) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.since = at
}

// requestSinceTime is the time recorded by setRequestSince.
func (t *turnState) requestSinceTime() time.Time {
	if t == nil {
		return time.Time{}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.since
}

// addLibraryRows remembers the outcome of the links of a call of add_to_library.
func (t *turnState) addLibraryRows(rows []addRow) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.libraryRows = append(t.libraryRows, rows...)
}

// libraryReport is the line for the person about the links that were not added, or "".
func (t *turnState) libraryReport() string {
	if t == nil {
		return ""
	}
	t.mu.Lock()
	rows := slices.Clone(t.libraryRows)
	t.mu.Unlock()
	return libraryReport(rows)
}

// setUserURLs records the addresses the person wrote in the conversation.
func (t *turnState) setUserURLs(keys map[string]string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.userURLs = keys
}

// resolveURL maps a link the model sent to the link the person wrote: the same address (the
// same key, urlKey), or the one link of the person that it differs from by a few characters.
// A model that copies 21 long links by hand drops a character now and then (in the live runs
// of 2026-11-20 it dropped "i:2" from one in 4 of 6 runs); the link it meant is a link the
// person wrote, so fetching that one gives away nothing. corrected tells that it was not the
// same. ok is false when the person wrote no such link, or two of them are as near.
func (t *turnState) resolveURL(raw string) (link string, corrected, ok bool) {
	if t == nil {
		return "", false, false
	}
	key := urlKey(trimURLEnd(strings.TrimSpace(raw)))
	if key == "" {
		return "", false, false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if written, same := t.userURLs[key]; same {
		return written, false, true
	}
	link, ok = nearestLink(key, t.userURLs)
	return link, ok, ok
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

// has reports whether the turn has read text from this source.
func (t *turnState) has(source string) bool {
	if t == nil {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return slices.Contains(t.external, source)
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

// setAnsweredBy records which model answers this turn.
func (t *turnState) setAnsweredBy(by answeredBy) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.by = by
}

// answeredByWhom is the model that answered this turn (the zero value when not recorded).
func (t *turnState) answeredByWhom() answeredBy {
	if t == nil {
		return answeredBy{}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.by
}

// noteFileWrite records that a file tool wrote or added text.
func (t *turnState) noteFileWrite() {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.fileWrites++
}

// filesWritten is how many times a file tool wrote or added text in this turn.
func (t *turnState) filesWritten() int {
	if t == nil {
		return 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.fileWrites
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
