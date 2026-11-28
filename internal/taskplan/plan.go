// Package taskplan is the plan of a task the assistant works on in a conversation: a goal,
// steps, and for each step whether it is done, with the evidence that it is.
//
// Why it exists. The history of a conversation keeps the words of the turns and not the results
// of their tools, so a "continue" started from the model's own earlier words, and that is where
// it wrote "extractions" of papers it never opened (the chat of 2026-11-21). The plan is kept
// by the server and put in front of the model in every turn of the conversation while it is
// active. And a step is "done" only when the server saw a successful call of the tool the step
// names since the plan was made: the ledger of the plan holds what the tools did, and a step
// that names a tool takes one unused entry of it as its evidence. The model's word is not enough,
// which makes "did you manage?" a lookup and not a thing to guess from words.
//
// This file is the plan itself (no database); store.go keeps it.
package taskplan

import (
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

// The limits of a plan. A plan is read by the model in every turn, so it is kept small.
const (
	MaxSteps     = 25
	MaxGoalRunes = 200
	MaxStepRunes = 160
	MaxNoteRunes = 200
	// MaxLedger is how many entries of the evidence the plan keeps; the oldest used one goes first.
	MaxLedger = 40
	// MaxEvidenceRunes cuts the line that says what a tool did.
	MaxEvidenceRunes = 140

	// What the note for the model shows of each part (see Render).
	renderStepRunes     = 80
	renderEvidenceRunes = 50
	renderNoteRunes     = 50
	// A plan of more steps than this is shown in a window: the ones that are over, the failed ones,
	// the last done one, and the next ones (see Render).
	renderAllSteps  = 8
	renderNextSteps = 3
)

// Statuses of a step.
const (
	StepPending = "pending"
	StepDoing   = "doing"
	StepDone    = "done"
	StepFailed  = "failed"
	StepSkipped = "skipped"
)

// Statuses of a plan.
const (
	// PlanProposed is a plan the server made and walks itself (Kind is set), waiting for the
	// person's yes; it is not run and not shown to the model.
	PlanProposed  = "proposed"
	PlanActive    = "active"
	PlanDone      = "done"
	PlanCancelled = "cancelled"
)

// ToolNone is the tool of a step that needs no tool: a decision, or the answer to the user. Such
// a step is done on the word of the model. A step that names no tool at all is not that: it needs
// some tool to have worked since the last step (see Update).
const ToolNone = "none"

// StepTools are the tools a step may name: the ones whose success the server can see, and none.
var StepTools = []string{"web_search", "web_fetch", "add_to_library", "list_documents", "search_documents", "read_attachment", "read_file", "write_file", ToolNone}

// InvalidError is a call the model got wrong; its message says how to fix it.
type InvalidError struct{ Reason string }

func (e *InvalidError) Error() string { return e.Reason }

// StepInput is a step as the model gives it.
type StepInput struct {
	Text string `json:"text"`
	Tool string `json:"tool,omitempty"`
}

// Step is a step of a plan.
type Step struct {
	ID     int    `json:"id"`
	Text   string `json:"text"`
	Tool   string `json:"tool,omitempty"`
	Status string `json:"status"`
	// Evidence is what the tool did, for a step done with a tool.
	Evidence string `json:"evidence,omitempty"`
	// Note is the reason of a failed or skipped step, or what the model said of a done one.
	Note string `json:"note,omitempty"`
	// Ref is what a step of a plan the server walks works on: the id of a document of the library.
	Ref string `json:"ref,omitempty"`
	// Parts and PartsDone are how far the reading of a whole document is (a task of the kind
	// "full", package taskrun): the document is read in Parts parts, PartsDone of them are written;
	// File is the file the next part goes to. Zero for every other step.
	Parts     int    `json:"parts,omitempty"`
	PartsDone int    `json:"parts_done,omitempty"`
	File      string `json:"file,omitempty"`
	// Link is the address a step of a task over links works on (package taskrun): until the server
	// has put it into the library Ref is empty; then Ref is the document and Added the time it was
	// added (the wait for its processing counts from then).
	Link  string     `json:"link,omitempty"`
	Added *time.Time `json:"added,omitempty"`
	// Folder is the file of the person's own folder a step of a task over the folder works on.
	Folder string `json:"folder,omitempty"`
	// About and Query are the subject of a research on the internet (package taskrun, the kind
	// "research") and the search that found the page of the step (Link): the notes of the page are
	// on that subject, and its passages nearest to both words are read.
	About string `json:"about,omitempty"`
	Query string `json:"query,omitempty"`
	// Expect is the title the document of a step must begin with: a link the search found for a
	// reference (agent/source_search.go) may be another work that cites it, and such a step fails.
	Expect string `json:"expect,omitempty"`
	// Title is the name the document of a step over a link gets in the library: the title of the
	// work of a reference; "" keeps the name of the file on the site.
	Title string `json:"title,omitempty"`
	// Also are other addresses of the same work (the copies the catalogue knows), tried in their order
	// when Link gives nothing.
	Also []string `json:"also,omitempty"`
}

// Evidence is an entry of the ledger: a successful call of a tool made after the plan.
type Evidence struct {
	N    int    `json:"n"`
	Tool string `json:"tool"`
	Gist string `json:"gist"`
	// Used: a step has taken it as its evidence.
	Used bool `json:"used,omitempty"`
}

// Plan is the plan of a task.
type Plan struct {
	ID     int64      `json:"id"`
	Goal   string     `json:"goal"`
	Steps  []Step     `json:"steps"`
	Ledger []Evidence `json:"-"`
	Status string     `json:"status"`
	// Kind is "" for a plan of the model, else the recipe the server walks (package taskrun).
	Kind      string    `json:"kind,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	// Warnings are what New mended in the plan it was given (not stored; for the model).
	Warnings []string `json:"-"`
}

// New makes a plan from the goal and the steps the model gave. A model that writes a long step, a
// thirteenth step or a tool the plan does not know has not made a mistake worth refusing the plan
// for (the replay of 2026-11-21 lost 4 of 6 plans to such refusals, and the model went on in words
// with none): the text is cut, the steps over the limit are left out and the tool is dropped, and
// what was done goes to Plan.Warnings, which the answer to the model carries. What cannot be
// mended is refused: no goal, no steps, a step with no text.
func New(goal string, inputs []StepInput) (*Plan, error) {
	goal = strings.TrimSpace(goal)
	if goal == "" {
		return nil, &InvalidError{`missing "goal": a short sentence about what the task is`}
	}
	if len(inputs) == 0 {
		return nil, &InvalidError{`missing "steps": a list of steps, each {"text": "...", "tool": "..."}`}
	}
	p := &Plan{Goal: goal, Status: PlanActive, Steps: make([]Step, 0, min(len(inputs), MaxSteps))}
	if utf8.RuneCountInString(goal) > MaxGoalRunes {
		p.Goal = cut(goal, MaxGoalRunes)
		p.Warnings = append(p.Warnings, fmt.Sprintf("the goal was cut to %d characters", MaxGoalRunes))
	}
	if len(inputs) > MaxSteps {
		p.Warnings = append(p.Warnings, fmt.Sprintf("only the first %d of %d steps were kept: group the work, for example one step for all the links", MaxSteps, len(inputs)))
		inputs = inputs[:MaxSteps]
	}
	var longSteps, droppedTools []string
	for i, in := range inputs {
		text := strings.TrimSpace(in.Text)
		if text == "" {
			return nil, &InvalidError{fmt.Sprintf("step %d has no text", i+1)}
		}
		if utf8.RuneCountInString(text) > MaxStepRunes {
			text = cut(text, MaxStepRunes)
			longSteps = append(longSteps, fmt.Sprint(i+1))
		}
		tool := strings.TrimSpace(in.Tool)
		if tool != "" && !slices.Contains(StepTools, tool) {
			droppedTools = append(droppedTools, fmt.Sprintf("step %d: %q", i+1, tool))
			tool = ""
		}
		p.Steps = append(p.Steps, Step{ID: i + 1, Text: text, Tool: tool, Status: StepPending})
	}
	if len(longSteps) > 0 {
		p.Warnings = append(p.Warnings, fmt.Sprintf("steps %s were cut to %d characters: a step is one short action", strings.Join(longSteps, ", "), MaxStepRunes))
	}
	if len(droppedTools) > 0 {
		p.Warnings = append(p.Warnings, fmt.Sprintf("these tools are not known and were left out (%s); the tools of a step are %s", strings.Join(droppedTools, ", "), strings.Join(StepTools, ", ")))
	}
	return p, nil
}

// AddEvidence records what a tool did. The ledger keeps MaxLedger entries.
func (p *Plan) AddEvidence(tool, gist string) {
	next := 1
	for _, e := range p.Ledger {
		next = max(next, e.N+1)
	}
	p.Ledger = append(p.Ledger, Evidence{N: next, Tool: tool, Gist: cut(gist, MaxEvidenceRunes)})
	for len(p.Ledger) > MaxLedger {
		drop := slices.IndexFunc(p.Ledger, func(e Evidence) bool { return e.Used })
		if drop < 0 {
			drop = 0
		}
		p.Ledger = slices.Delete(p.Ledger, drop, drop+1)
	}
}

// Accepts tells whether a call of tool is evidence for a step that names stepTool: the same
// tool, and for a write either of the two ways of writing.
func Accepts(stepTool, tool string) bool {
	if stepTool == tool {
		return true
	}
	return stepTool == "write_file" && tool == "append_file"
}

// Outcome is what an update of a step tells the model.
type Outcome struct {
	Message string
	// Finished: no step is left to do; the plan is done.
	Finished bool
}

// Update sets the status of a step. "done" for a step that names a tool needs an unused entry of
// the ledger of that tool; "failed" and "skipped" need the reason. The error is for the model.
func (p *Plan) Update(stepID int, status, note string) (Outcome, error) {
	if p.Status != PlanActive {
		return Outcome{}, &InvalidError{"this plan is over; make a new one with plan_set"}
	}
	i := slices.IndexFunc(p.Steps, func(s Step) bool { return s.ID == stepID })
	if i < 0 {
		return Outcome{}, &InvalidError{fmt.Sprintf("there is no step %d; the steps are 1 to %d", stepID, len(p.Steps))}
	}
	step := &p.Steps[i]
	note = strings.TrimSpace(note)
	if n := utf8.RuneCountInString(note); n > MaxNoteRunes {
		note = cut(note, MaxNoteRunes)
	}

	switch status {
	case StepDoing:
		if step.Status == StepDone {
			return Outcome{}, &InvalidError{fmt.Sprintf("step %d is done already", stepID)}
		}
		step.Status = StepDoing
		return Outcome{Message: fmt.Sprintf("Step %d is marked as in progress. Do it now with its tool.", stepID)}, nil

	case StepDone:
		if step.Status == StepDone {
			return Outcome{Message: fmt.Sprintf("Step %d was done already.%s", stepID, p.nextHint())}, nil
		}
		if step.Tool != ToolNone {
			// A step that names a tool needs a call of that tool; one that names none, some call.
			e := slices.IndexFunc(p.Ledger, func(e Evidence) bool { return !e.Used && (step.Tool == "" || Accepts(step.Tool, e.Tool)) })
			if e < 0 {
				what := "successful call of " + step.Tool
				if step.Tool == "" {
					what = "successful call of any tool"
				}
				return Outcome{}, &InvalidError{fmt.Sprintf(
					"NOT done: no %s is on record for step %d (a call counts for one step only, and only after the plan was made). "+
						"Do the step with a tool first. If it cannot be done, mark it failed with the reason, or skipped.", what, stepID)}
			}
			p.Ledger[e].Used = true
			step.Evidence = p.Ledger[e].Gist
		}
		step.Status, step.Note = StepDone, note

	case StepFailed, StepSkipped:
		if note == "" {
			return Outcome{}, &InvalidError{fmt.Sprintf("say why step %d is %s: call plan_update again with a note", stepID, status)}
		}
		if step.Status == StepDone {
			return Outcome{}, &InvalidError{fmt.Sprintf("step %d is done already", stepID)}
		}
		step.Status, step.Note = status, note

	default:
		return Outcome{}, &InvalidError{fmt.Sprintf("the status %q is not one of doing, done, failed, skipped", status)}
	}

	if p.next() == nil {
		p.Status = PlanDone
		return Outcome{
			Message:  fmt.Sprintf("Step %d is %s. Every step is finished: tell the user in a few sentences what was done, what failed and why. Do not claim more than the list says.", stepID, step.Status),
			Finished: true,
		}, nil
	}
	return Outcome{Message: fmt.Sprintf("Step %d is %s.%s", stepID, step.Status, p.nextHint())}, nil
}

// next is the first step that is not finished, or nil.
func (p *Plan) next() *Step {
	for i := range p.Steps {
		switch p.Steps[i].Status {
		case StepPending, StepDoing:
			return &p.Steps[i]
		}
	}
	return nil
}

// Next is the first step that is not finished, or nil when none is left.
func (p *Plan) Next() *Step { return p.next() }

func (p *Plan) nextHint() string {
	n := p.next()
	if n == nil {
		return ""
	}
	hint := fmt.Sprintf(" Next is step %d: %s.", n.ID, cut(n.Text, renderStepRunes))
	if n.Tool != "" && n.Tool != ToolNone {
		hint += " Use " + n.Tool + "."
	}
	return hint + " Do it now."
}

// Render is the note put next to the last message of the person, in the copy the model reads: the
// plan as it stands, and how to work with it. It is never stored. Cyrillic costs about a token
// for two letters, so each part is cut short, and a plan of many steps is shown in a window (a
// plan of 25 steps in full would take a quarter of the room of the history): how far it is, the
// steps that failed or were skipped, the last one done, and the next ones.
func Render(p *Plan) string {
	var b strings.Builder
	b.WriteString("\n\n[Plan of the task, kept by the system - not written by the user]\nGoal: " + cut(p.Goal, MaxGoalRunes) + "\n")
	next := p.next()

	line := func(s Step) {
		word := s.Status
		switch {
		case next != nil && s.ID == next.ID && s.Status == StepPending:
			word = "NEXT"
		case s.Status == StepPending:
			word = "todo"
		}
		fmt.Fprintf(&b, "%d. %s - %s", s.ID, word, cut(s.Text, renderStepRunes))
		if s.Tool != "" && s.Tool != ToolNone && (s.Status == StepPending || s.Status == StepDoing) {
			b.WriteString(" [tool: " + s.Tool + "]")
		}
		switch {
		case s.Status == StepDone && s.Evidence != "":
			b.WriteString(" (" + cut(s.Evidence, renderEvidenceRunes) + ")")
		case (s.Status == StepFailed || s.Status == StepSkipped) && s.Note != "":
			b.WriteString(" (" + cut(s.Note, renderNoteRunes) + ")")
		}
		b.WriteString("\n")
	}

	if len(p.Steps) <= renderAllSteps {
		for _, s := range p.Steps {
			line(s)
		}
	} else {
		var done, failed, skipped int
		for _, s := range p.Steps {
			switch s.Status {
			case StepDone:
				done++
			case StepFailed:
				failed++
			case StepSkipped:
				skipped++
			}
		}
		fmt.Fprintf(&b, "Progress: %d of %d steps done, %d failed, %d skipped.\n", done, len(p.Steps), failed, skipped)
		lastDone := -1
		for i, s := range p.Steps {
			if s.Status == StepDone {
				lastDone = i
			}
		}
		shown, ahead := 0, 0
		for i, s := range p.Steps {
			switch {
			case s.Status == StepFailed || s.Status == StepSkipped, i == lastDone:
				line(s)
			case s.Status == StepPending || s.Status == StepDoing:
				if shown < renderNextSteps {
					line(s)
					shown++
				} else {
					ahead++
				}
			}
		}
		if ahead > 0 {
			fmt.Fprintf(&b, "(%d more steps to do after these)\n", ahead)
		}
	}
	b.WriteString("How to work: do the NEXT step now with its tool, then call plan_update for it. A step is done only after a successful call of its tool; " +
		"the system checks. If a step cannot be done (a 404, a page that is only a summary), mark it failed with the reason, go on, and tell the user plainly. " +
		"Never write what a source says if you did not open it. When the user asks how far you are, answer from this list.]")
	return b.String()
}

// cut shortens s to at most max runes, marking the cut.
func cut(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	r := []rune(s)
	return string(r[:max-1]) + "…"
}
