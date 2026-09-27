package limn

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
)

const evidenceRelevanceThreshold = 0.60

func hasRelevantEvidence(ctx context.Context, mem MemoryNode, toolCalls []ToolCallEvidence) (bool, string, error) {
	if len(toolCalls) == 0 {
		return false, "", nil
	}

	claimEmbedding, err := getEmbedding(ctx, mem.Summary)
	if err != nil {
		return false, "", fmt.Errorf("failed to embed claim for evidence check: %w", err)
	}

	for _, tc := range toolCalls {
		if tc.DiffText == "" {
			continue
		}
		diffEmbedding, err := getEmbedding(ctx, tc.DiffText)
		if err != nil {
			continue
		}
		if cosineSimilarity(claimEmbedding, diffEmbedding) >= evidenceRelevanceThreshold {
			return true, tc.Name, nil
		}
	}
	return false, "", nil
}

func PromoteStandardMemory(ctx context.Context, db *sql.DB, mem MemoryNode, toolCalls []ToolCallEvidence) (int, error) {
	relevant, evidenceSource, err := hasRelevantEvidence(ctx, mem, toolCalls)
	if err != nil {
		return 0, fmt.Errorf("evidence check failed: %w", err)
	}

	candidateMatches, err := findSimilarAuthoritative(ctx, db, mem.Embedding, correctionMatchThreshold)
	if err != nil {
		return 0, fmt.Errorf("similarity search failed: %w", err)
	}

	if mem.Provenance == nil {
		mem.Provenance = make(map[string]interface{})
	}
	mem.Provenance["candidate_targets"] = candidateMatches
	if relevant {
		mem.Provenance["evidence_source"] = evidenceSource
	}

	switch {
	case relevant && len(candidateMatches) == 1:
		return SupersedeMemory(ctx, db, candidateMatches[0].ID, mem)
	case relevant && len(candidateMatches) == 0:
		mem.State = "AUTHORITATIVE"
		return insertMemory(ctx, db, mem)
	default:
		if mem.State == "" {
			mem.State = "CANDIDATE"
		}
		return insertMemory(ctx, db, mem)
	}
}

// SupersedeMemory inserts a brand-new AUTHORITATIVE memory and marks an
// existing one SUPERSEDED. Used by the automatic write path, where the
// new memory doesn't exist as a row yet. NOT used for confirming an
// already-inserted PENDING row — see ConfirmPendingMemory below, which
// promotes that row in place instead of inserting a duplicate.
func SupersedeMemory(ctx context.Context, db *sql.DB, oldID int, newMem MemoryNode) (int, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback()

	var newID int
	provenanceJSON, _ := json.Marshal(newMem.Provenance)
	embeddingJSON, _ := json.Marshal(newMem.Embedding)

	err = tx.QueryRowContext(ctx, `
		INSERT INTO limn_memories (state, confidence, category, summary, rationale, embedding, provenance)
		VALUES ('AUTHORITATIVE', $1, $2, $3, $4, $5, $6)
		RETURNING id;`,
		newMem.Confidence, newMem.Category, newMem.Summary, newMem.Rationale, string(embeddingJSON), provenanceJSON,
	).Scan(&newID)
	if err != nil {
		return 0, fmt.Errorf("failed to insert new authoritative memory: %w", err)
	}

	if _, err = tx.ExecContext(ctx, `UPDATE limn_memories SET state = 'SUPERSEDED' WHERE id = $1`, oldID); err != nil {
		return 0, fmt.Errorf("failed to mark old memory superseded: %w", err)
	}

	if _, err = tx.ExecContext(ctx, `INSERT INTO limn_edges (source_id, target_id, relationship) VALUES ($1, $2, 'supersedes')`, newID, oldID); err != nil {
		return 0, fmt.Errorf("failed to write supersedes edge: %w", err)
	}

	if _, err = tx.ExecContext(ctx, `
		UPDATE limn_memories SET state = 'NEEDS_REVALIDATION'
		WHERE id IN (SELECT source_id FROM limn_edges WHERE target_id = $1 AND relationship = 'depends_on')`, oldID); err != nil {
		return 0, fmt.Errorf("failed to invalidate dependent nodes: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("transaction commit failed: %w", err)
	}
	return newID, nil
}

func insertMemory(ctx context.Context, db *sql.DB, mem MemoryNode) (int, error) {
	var id int
	provenanceJSON, _ := json.Marshal(mem.Provenance)
	embeddingJSON, _ := json.Marshal(mem.Embedding)

	err := db.QueryRowContext(ctx, `
		INSERT INTO limn_memories (state, confidence, category, summary, rationale, embedding, provenance)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		RETURNING id;`,
		mem.State, mem.Confidence, mem.Category, mem.Summary, mem.Rationale, string(embeddingJSON), provenanceJSON,
	).Scan(&id)
	return id, err
}

// ConfirmPendingMemory promotes an existing PENDING or PENDING_CONFIRMATION
// row to AUTHORITATIVE in place. If its provenance names exactly one
// candidate target (from an earlier ambiguous or conversational
// correction), that target is atomically marked SUPERSEDED, linked via a
// 'supersedes' edge, and its dependents flagged NEEDS_REVALIDATION —
// mirroring what SupersedeMemory does for the automatic path, but without
// inserting a duplicate row, since this memory already exists.
func ConfirmPendingMemory(ctx context.Context, db *sql.DB, pendingID int) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback()

	var state string
	var provenanceJSON []byte
	err = tx.QueryRowContext(ctx, `
		SELECT state, provenance FROM limn_memories WHERE id = $1 FOR UPDATE`, pendingID,
	).Scan(&state, &provenanceJSON)
	if err != nil {
		return fmt.Errorf("failed to fetch pending memory #%d: %w", pendingID, err)
	}
	if state != "PENDING" && state != "PENDING_CONFIRMATION" {
		return fmt.Errorf("memory #%d is not pending (state=%s); refusing to confirm", pendingID, state)
	}

	var prov map[string]interface{}
	if len(provenanceJSON) > 0 {
		if err := json.Unmarshal(provenanceJSON, &prov); err != nil {
			return fmt.Errorf("failed to parse provenance for #%d: %w", pendingID, err)
		}
	}

	var targetID int
	hasSingleTarget := false
	if rawTargets, ok := prov["candidate_targets"]; ok {
		targetsJSON, _ := json.Marshal(rawTargets)
		var targets []MatchTarget
		if err := json.Unmarshal(targetsJSON, &targets); err == nil && len(targets) == 1 {
			targetID = targets[0].ID
			hasSingleTarget = true
		}
	}

	if hasSingleTarget {
		if _, err := tx.ExecContext(ctx, `UPDATE limn_memories SET state = 'SUPERSEDED' WHERE id = $1`, targetID); err != nil {
			return fmt.Errorf("failed to mark target #%d superseded: %w", targetID, err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO limn_edges (source_id, target_id, relationship) VALUES ($1, $2, 'supersedes')`, pendingID, targetID); err != nil {
			return fmt.Errorf("failed to write supersedes edge: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE limn_memories SET state = 'NEEDS_REVALIDATION'
			WHERE id IN (SELECT source_id FROM limn_edges WHERE target_id = $1 AND relationship = 'depends_on')`, targetID); err != nil {
			return fmt.Errorf("failed to invalidate dependent nodes: %w", err)
		}
	}

	if _, err := tx.ExecContext(ctx, `UPDATE limn_memories SET state = 'AUTHORITATIVE' WHERE id = $1`, pendingID); err != nil {
		return fmt.Errorf("failed to promote memory #%d: %w", pendingID, err)
	}

	return tx.Commit()
}
