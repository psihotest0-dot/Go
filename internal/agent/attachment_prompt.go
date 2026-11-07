package agent

import (
	"context"
	"log/slog"
	"sort"

	"github.com/google/uuid"

	"go-monolith/internal/attachments"
)

const (
	// maxInlineAttachmentTokens is how much of the attached files the prompt shows in
	// full. The window of one request is 16 384 tokens and the fixed part, the answer and
	// the history already take most of it; this is what can be spared (about 12 KB of
	// code, 300 lines). The rest of a big file is read through read_attachment.
	maxInlineAttachmentTokens = 4000

	// attachmentSectionTokens is the header and the rules of the section;
	// attachmentEntryTokens is one entry of the list of files.
	attachmentSectionTokens = 220
	attachmentEntryTokens   = 45

	// The room taken for the files comes from the history and from the dynamic part of the
	// prompt (memories, book summaries), never below these floors: a conversation about a
	// file still needs its last few messages.
	minHistoryTokensWithFiles       = 1200
	minPromptContextTokensWithFiles = 900
)

// promptFile is an attached file as the system prompt shows it.
type promptFile struct {
	Name    string
	Lines   int
	Tokens  int
	Version int
	// Inline is the numbered text of the file; empty when the file does not fit and
	// the model has to read it through read_attachment.
	Inline string
}

// attachmentPlan says what the prompt of a turn contains about attached files and what
// the budgets are once the room for them has been taken.
type attachmentPlan struct {
	files  []promptFile
	limits budgets
}

// hasText reports whether the text of some file is in the prompt.
func (p attachmentPlan) hasText() bool {
	for _, f := range p.files {
		if f.Inline != "" {
			return true
		}
	}
	return false
}

// forAttachments takes room for want tokens of attached files from the history and the
// dynamic part of the prompt (a third from the second, the rest from the first), and
// returns the new budgets and how many tokens it really could take.
func (b budgets) forAttachments(want int) (budgets, int) {
	spareHistory := max(0, b.historyTokens-minHistoryTokensWithFiles)
	sparePrompt := max(0, b.promptContextTokens-minPromptContextTokensWithFiles)
	granted := max(0, min(want, spareHistory+sparePrompt))

	fromPrompt := min(granted/3, sparePrompt)
	fromHistory := granted - fromPrompt
	if fromHistory > spareHistory {
		fromHistory = spareHistory
		fromPrompt = granted - fromHistory
	}
	b.promptContextTokens -= fromPrompt
	b.historyTokens -= fromHistory
	return b, granted
}

// planAttachments looks at the files attached to the session and decides which are shown
// in full: the newest first, as long as they fit what can be spared. A failure to read
// them is logged and the turn goes on without files - the question is more important
// than the attachment.
func (o *Orchestrator) planAttachments(ctx context.Context, userID uuid.UUID, sessionID string, lim budgets) attachmentPlan {
	plan := attachmentPlan{limits: lim}
	if o.attachments == nil {
		return plan
	}
	files, err := o.attachments.Files(ctx, userID, sessionID)
	if err != nil {
		slog.WarnContext(ctx, "failed to read the attached files; answering without them", "err", err, "session_id", sessionID)
		return plan
	}
	if len(files) == 0 {
		return plan
	}

	overhead := attachmentSectionTokens + attachmentEntryTokens*len(files)
	_, grantable := lim.forAttachments(maxInlineAttachmentTokens + overhead)
	room := grantable - overhead

	// files are newest first: the file the user just sent gets the room first.
	shown := make(map[uuid.UUID]string, len(files))
	used := 0
	for _, f := range files {
		text := attachments.Inline(f)
		cost := attachments.EstimateTokens(len(text))
		if cost <= room-used {
			shown[f.ID] = text
			used += cost
		}
	}

	plan.limits, _ = lim.forAttachments(overhead + used)

	sort.SliceStable(files, func(i, j int) bool { return files[i].CreatedAt.Before(files[j].CreatedAt) })
	for _, f := range files {
		plan.files = append(plan.files, promptFile{Name: f.Name, Lines: f.Lines, Tokens: f.Tokens, Version: f.Version, Inline: shown[f.ID]})
	}
	return plan
}
