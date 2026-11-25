package agent

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"

	"go-monolith/metrics"
)

// The texts the server writes on its own in a conversation shared with Telegram - a reminder, the
// monthly report of the bills, the report of a task - may come while the bot is not there: the
// sender of the reminders makes its first pass as the backend starts, and the bot sets itself only
// after Telegram has answered (0.2-0.6 s later in the log of 2026-11-25, minutes when Telegram cannot
// be reached). Such a text was dropped, and the reminder was marked sent all the same (the code
// review of 2026-11-25). Now it waits here, in memory, and goes out when the bot sets itself. A
// crash in between loses it, as a crash between the database and Telegram always did.

const (
	// maxPendingOutside bounds the texts that wait; the oldest goes first.
	maxPendingOutside = 50
	// pendingOutsideFor: a text that waited longer is old news and is not sent.
	pendingOutsideFor = 24 * time.Hour
)

type pendingText struct {
	userID  uuid.UUID
	session string
	text    string
	at      time.Time
}

// outsideQueue holds the texts that came while nobody was set to send them.
type outsideQueue struct {
	mu    sync.Mutex
	texts []pendingText
	// now is the clock (time.Now when nil).
	now func() time.Time
	// wg waits for the sending of the texts that waited (tests).
	wg sync.WaitGroup
}

func (q *outsideQueue) clock() time.Time {
	if q.now != nil {
		return q.now()
	}
	return time.Now()
}

// holdOutside keeps a text until a notifier is set; when one was set meanwhile it is returned to send at
// once.
func (o *Orchestrator) holdOutside(userID uuid.UUID, session, text string) (OutsideNotifier, bool) {
	q := &o.pendingOutside
	q.mu.Lock()
	defer q.mu.Unlock()
	if n := o.outside.Load(); n != nil {
		return *n, true
	}
	if len(q.texts) == maxPendingOutside {
		q.texts = q.texts[1:]
		metrics.TelegramMirror.WithLabelValues("dropped").Inc()
	}
	q.texts = append(q.texts, pendingText{userID: userID, session: session, text: text, at: q.clock()})
	metrics.TelegramMirror.WithLabelValues("held").Inc()
	slog.Info("a text for Telegram waits for the bot", "waiting", len(q.texts))
	return nil, false
}

// sendHeldOutside sends the texts that waited with n, the oldest first, in the background: a slow
// Telegram must not hold the bot that has just set itself.
func (o *Orchestrator) sendHeldOutside(n OutsideNotifier) {
	q := &o.pendingOutside
	q.mu.Lock()
	texts := q.texts
	q.texts = nil
	q.mu.Unlock()
	if len(texts) == 0 {
		return
	}
	now := q.clock()
	q.wg.Add(1)
	go func() {
		defer q.wg.Done()
		for _, p := range texts {
			if now.Sub(p.at) > pendingOutsideFor {
				metrics.TelegramMirror.WithLabelValues("expired").Inc()
				continue
			}
			o.sendOutside(context.Background(), n, p.userID, p.session, p.text)
		}
		slog.Info("the texts for Telegram that waited for the bot were sent", "count", len(texts))
	}()
}
