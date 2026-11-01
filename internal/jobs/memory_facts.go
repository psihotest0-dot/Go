package jobs

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	pgvector "github.com/pgvector/pgvector-go"
	"github.com/sqlc-dev/pqtype"

	"go-monolith/internal/config"
	"go-monolith/internal/db"
)

// factAction is what to do with one fact found in a message.
type factAction struct {
	// store is false when the fact needs no change at all (it is already known).
	store bool
	// status is the status the memory for the fact gets: active or candidate.
	status string
	// supersede lists the memories that stop being in force because of it.
	supersede []uuid.UUID
}

// memoryValue is the "value" the model recorded for a stored fact.
func memoryValue(m db.Memory) string {
	if !m.Metadata.Valid {
		return ""
	}
	var meta struct {
		Value string `json:"value"`
	}
	_ = json.Unmarshal(m.Metadata.RawMessage, &meta)
	return strings.TrimSpace(meta.Value)
}

// decideFact applies the rules for a fact against what is already stored.
//
// stored is the memory with the very same statement (same content hash), if
// there is one, in whatever status. live holds the memories in force (active or
// candidate) that share the fact's fact_key.
//
//   - An identical statement that is already active changes nothing.
//   - A fact nobody contradicts becomes active.
//   - An explicit change ("Преместих се във Варна") becomes active and replaces
//     what it contradicts.
//   - A silent contradiction only becomes a candidate: the active fact stays in
//     force, so two facts that exclude each other are never both active. An older
//     candidate for the same key is replaced, the newest unconfirmed claim wins.
//   - A candidate becomes active when the user says it again: the same
//     statement, or the same value in other words. That confirmation replaces
//     the fact it contradicted.
func decideFact(fact Fact, stored *db.Memory, live []db.Memory) factAction {
	if stored != nil && stored.Status == "active" {
		return factAction{}
	}

	var others []db.Memory
	for _, m := range live {
		if stored == nil || m.ID != stored.ID {
			others = append(others, m)
		}
	}
	idsOf := func(ms []db.Memory, onlyCandidates bool) []uuid.UUID {
		var ids []uuid.UUID
		for _, m := range ms {
			if !onlyCandidates || m.Status == "candidate" {
				ids = append(ids, m.ID)
			}
		}
		return ids
	}

	switch {
	case len(others) == 0:
		return factAction{store: true, status: "active"}
	case fact.IsExplicitChange:
		return factAction{store: true, status: "active", supersede: idsOf(others, false)}
	case stored != nil && stored.Status == "candidate":
		return factAction{store: true, status: "active", supersede: idsOf(others, false)}
	}

	value := strings.TrimSpace(fact.Value)
	if value != "" {
		for _, m := range others {
			if m.Status == "candidate" && strings.EqualFold(memoryValue(m), value) {
				return factAction{store: true, status: "active", supersede: idsOf(others, false)}
			}
		}
	}
	return factAction{store: true, status: "candidate", supersede: idsOf(others, true)}
}

// embedFact embeds the statement of a fact. It is done before a transaction is
// opened: the embedder may take up to the 30 s limit of processFact, and a
// transaction held open that long blocks every other writer of the rows it
// touched.
func (w *MemoryConsolidationWorker) embedFact(ctx context.Context, statement string) (pgvector.Vector, error) {
	vectors, err := w.embedder.EmbedTexts(ctx, []string{statement}, false)
	if err != nil {
		return pgvector.Vector{}, err
	}
	if len(vectors) == 0 {
		return pgvector.Vector{}, fmt.Errorf("embedder returned empty result")
	}
	if len(vectors[0]) != config.VectorDimension {
		return pgvector.Vector{}, fmt.Errorf("embedder returned invalid vector dimension: expected %d, got %d", config.VectorDimension, len(vectors[0]))
	}
	return pgvector.NewVector(vectors[0]), nil
}

// processFact stores one fact of a user message according to decideFact.
// The same statement is recognised by its content hash, so analysing a message
// twice (a retry after a failure half way through) cannot create a duplicate.
func (w *MemoryConsolidationWorker) processFact(ctx context.Context, msg db.Message, fact Fact) error {
	fact.Statement = strings.TrimSpace(fact.Statement)
	fact.FactKey = strings.TrimSpace(fact.FactKey)
	if fact.Statement == "" {
		return nil
	}
	hash := db.GenerateHash(fact.Statement)

	var stored *db.Memory
	existing, err := w.dbClient.Queries.GetMemoryByHash(ctx, db.GetMemoryByHashParams{UserID: msg.UserID, ContentHash: hash})
	switch {
	case err == nil:
		if existing.Status == "active" {
			return nil // already known; nothing to embed or write
		}
		stored = &existing
	case !errors.Is(err, sql.ErrNoRows):
		return err
	}

	var vec pgvector.Vector
	if stored == nil {
		if vec, err = w.embedFact(ctx, fact.Statement); err != nil {
			return err
		}
	}

	tx, err := w.dbClient.SqlDB.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	q := w.dbClient.Queries.WithTx(tx)

	var live []db.Memory
	if fact.FactKey != "" {
		live, err = q.GetLiveMemoriesByFactKey(ctx, db.GetLiveMemoriesByFactKeyParams{UserID: msg.UserID, FactKey: fact.FactKey})
		if err != nil {
			return err
		}
	}

	action := decideFact(fact, stored, live)
	if !action.store {
		return nil
	}
	for _, id := range action.supersede {
		if err := q.SupercedeMemory(ctx, db.SupercedeMemoryParams{ID: id, UserID: msg.UserID}); err != nil {
			return err
		}
	}

	if stored != nil {
		// The statement is already in the table as a candidate or as a fact that
		// was replaced earlier: bring that row back instead of inserting a copy.
		if err := q.SetMemoryStatus(ctx, db.SetMemoryStatusParams{ID: stored.ID, UserID: msg.UserID, Status: action.status}); err != nil {
			return err
		}
		return tx.Commit()
	}

	metaBytes, _ := json.Marshal(map[string]interface{}{
		"fact_key":           fact.FactKey,
		"value":              fact.Value,
		"is_explicit_change": fact.IsExplicitChange,
	})
	_, err = q.CreateMemory(ctx, db.CreateMemoryParams{
		UserID:          msg.UserID,
		Content:         fact.Statement,
		Status:          action.status,
		Embedding:       vec,
		EmbeddingModel:  config.DefaultEmbedModel,
		SourceMessageID: uuid.NullUUID{UUID: msg.ID, Valid: true},
		Importance:      1,
		Metadata:        pqtype.NullRawMessage{RawMessage: metaBytes, Valid: true},
		ContentHash:     hash,
	})
	if db.IsUniqueViolation(err) {
		// Someone stored the same statement between the lookup and the insert.
		return nil
	}
	if err != nil {
		return err
	}
	return tx.Commit()
}
