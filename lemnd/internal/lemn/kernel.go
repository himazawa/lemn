package lemn

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
)

type ConfirmationDecision struct {
	Relation            string
	TargetID            int
	DependsOn           []int
	ReplaceDependencies bool
}

const evidenceRelevanceThreshold = 0.60

type evidenceSignal struct {
	Relevant   bool
	Similarity float64 // best claim-vs-tool-output cosine, kept even below threshold for tuning
	Tool       string
}

func evidenceSignalFromScores(toolNames []string, similarities []float64) evidenceSignal {
	var signal evidenceSignal
	count := len(toolNames)
	if len(similarities) < count {
		count = len(similarities)
	}
	if count == 0 {
		return signal
	}

	signal.Tool = toolNames[0]
	signal.Similarity = similarities[0]
	for index := 1; index < count; index++ {
		if similarities[index] > signal.Similarity {
			signal.Tool = toolNames[index]
			signal.Similarity = similarities[index]
		}
	}
	signal.Relevant = signal.Similarity >= evidenceRelevanceThreshold
	return signal
}

// measureEvidence scans every tool call rather than stopping at the first
// match, so the logged similarity is the true best.
func measureEvidence(ctx context.Context, mem MemoryNode, toolCalls []ToolCallEvidence) (evidenceSignal, error) {
	var sig evidenceSignal
	if len(toolCalls) == 0 {
		return sig, nil
	}

	claimEmbedding := mem.Embedding
	if len(claimEmbedding) == 0 {
		var err error
		if claimEmbedding, err = getEmbedding(ctx, mem.Summary); err != nil {
			return sig, fmt.Errorf("failed to embed claim for evidence check: %w", err)
		}
	}

	toolNames := make([]string, 0, len(toolCalls))
	similarities := make([]float64, 0, len(toolCalls))
	for _, tc := range toolCalls {
		if tc.DiffText == "" {
			continue
		}
		diffEmbedding, err := getEmbedding(ctx, tc.DiffText)
		if err != nil {
			continue
		}
		toolNames = append(toolNames, tc.Name)
		similarities = append(similarities, cosineSimilarity(claimEmbedding, diffEmbedding))
	}
	return evidenceSignalFromScores(toolNames, similarities), nil
}

func insertMemory(ctx context.Context, db *sql.DB, mem MemoryNode) (int, error) {
	var id int
	provenanceJSON, _ := json.Marshal(mem.Provenance)
	embeddingJSON, _ := json.Marshal(mem.Embedding)

	err := db.QueryRowContext(ctx, `
		INSERT INTO lemn_memories (state, project_id, confidence, category, summary, rationale, embedding, provenance)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		RETURNING id;`,
		mem.State, NormalizeScope(mem.ProjectID), mem.Confidence, mem.Category, mem.Summary, mem.Rationale, string(embeddingJSON), provenanceJSON,
	).Scan(&id)
	return id, err
}

// ConfirmPendingMemory promotes a reviewed memory and applies its proposed
// relation and dependencies atomically.
func ConfirmPendingMemory(ctx context.Context, db *sql.DB, pendingID int, decisions ...ConfirmationDecision) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback()

	var state, projectID string
	var provenanceJSON []byte
	err = tx.QueryRowContext(ctx, `
		SELECT state, project_id, provenance FROM lemn_memories WHERE id = $1 FOR UPDATE`, pendingID,
	).Scan(&state, &projectID, &provenanceJSON)
	if err != nil {
		return fmt.Errorf("failed to fetch pending memory #%d: %w", pendingID, err)
	}
	switch state {
	case "OBSERVED", "CANDIDATE", "PENDING", "PENDING_CONFIRMATION", "NEEDS_REVALIDATION":
	default:
		return fmt.Errorf("memory #%d is not awaiting review (state=%s)", pendingID, state)
	}

	var prov map[string]interface{}
	if len(provenanceJSON) > 0 {
		if err := json.Unmarshal(provenanceJSON, &prov); err != nil {
			return fmt.Errorf("failed to parse provenance for #%d: %w", pendingID, err)
		}
	}
	if prov == nil {
		prov = make(map[string]interface{})
	}
	decision := ConfirmationDecision{}
	if len(decisions) > 0 {
		decision = decisions[0]
	}
	if decision.Relation != "" {
		if decision.Relation != "independent" && decision.Relation != "supersedes" && decision.Relation != "contradicts" {
			return fmt.Errorf("invalid relation %q", decision.Relation)
		}
		prov["proposed_relation"] = decision.Relation
	}
	if decision.TargetID > 0 {
		prov["proposed_target_id"] = decision.TargetID
	}
	if decision.ReplaceDependencies {
		prov["depends_on"] = decision.DependsOn
	}

	targetID := jsonInt(prov["proposed_target_id"])
	relation, hasProposedRelation := prov["proposed_relation"].(string)
	if !hasProposedRelation {
		relation = "independent"
		if jsonInt(prov["kernel_schema_version"]) < 2 && (state == "PENDING" || state == "PENDING_CONFIRMATION") {
			if rawTargets, ok := prov["candidate_targets"]; ok {
				targetsJSON, _ := json.Marshal(rawTargets)
				var targets []MatchTarget
				if err := json.Unmarshal(targetsJSON, &targets); err == nil && len(targets) == 1 {
					targetID = targets[0].ID
					relation = "supersedes"
				}
			}
		}
	}
	if relation != "supersedes" && relation != "contradicts" {
		relation = "independent"
	}
	if decision.TargetID > 0 && relation == "independent" {
		return fmt.Errorf("memory #%d has a target override but no supersedes/contradicts relation", pendingID)
	}
	if relation != "independent" && targetID <= 0 {
		return fmt.Errorf("memory #%d proposes %s without a valid target; review its provenance before confirming", pendingID, relation)
	}
	if relation == "independent" {
		delete(prov, "proposed_target_id")
		delete(prov, "proposed_relation")
		targetID = 0
	}
	prov["confirmed_relation"] = relation
	// Dependencies are validated before the target flip on purpose: the flip's
	// cascade demotes the target's dependents, and the confirmer may itself
	// depend on one of them (its supersedes target's dependent). Validating
	// after the cascade would let a confirmation fail on a dependency its own
	// transaction just demoted, rolling back the flip it caused.
	dependencies := jsonIntSlice(prov["depends_on"])
	sort.Ints(dependencies)
	if _, err := tx.ExecContext(ctx, `DELETE FROM lemn_edges WHERE source_id = $1 AND relationship = 'depends_on'`, pendingID); err != nil {
		return fmt.Errorf("failed to replace dependency edges: %w", err)
	}
	for _, dependencyID := range dependencies {
		if dependencyID <= 0 || dependencyID == pendingID || dependencyID == targetID {
			return fmt.Errorf("invalid dependency id %d for memory #%d", dependencyID, pendingID)
		}
		var dependencyState, dependencyScope string
		if err := tx.QueryRowContext(ctx, `SELECT state, project_id FROM lemn_memories WHERE id = $1 FOR UPDATE`, dependencyID).Scan(&dependencyState, &dependencyScope); err != nil {
			return fmt.Errorf("failed to lock dependency #%d: %w", dependencyID, err)
		}
		allowedScope := dependencyScope == projectID || (projectID != GlobalScope && dependencyScope == GlobalScope)
		if dependencyState != "AUTHORITATIVE" || !allowedScope {
			return fmt.Errorf("dependency #%d is not authoritative and visible in scope %q", dependencyID, projectID)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO lemn_edges (source_id, target_id, relationship) VALUES ($1, $2, 'depends_on') ON CONFLICT DO NOTHING`, pendingID, dependencyID); err != nil {
			return fmt.Errorf("failed to write dependency edge to #%d: %w", dependencyID, err)
		}
	}

	if targetID > 0 && relation != "independent" {
		var targetState, targetScope string
		if err := tx.QueryRowContext(ctx, `SELECT state, project_id FROM lemn_memories WHERE id = $1 FOR UPDATE`, targetID).Scan(&targetState, &targetScope); err != nil {
			return fmt.Errorf("failed to lock relation target #%d: %w", targetID, err)
		}
		if targetState != "AUTHORITATIVE" || targetScope != projectID {
			return fmt.Errorf("relation target #%d is no longer authoritative in scope %q", targetID, projectID)
		}
		targetState = "SUPERSEDED"
		if relation == "contradicts" {
			targetState = "CONTRADICTED"
		}
		if _, err := tx.ExecContext(ctx, `UPDATE lemn_memories SET state = $1 WHERE id = $2`, targetState, targetID); err != nil {
			return fmt.Errorf("failed to mark target #%d %s: %w", targetID, targetState, err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO lemn_edges (source_id, target_id, relationship) VALUES ($1, $2, $3) ON CONFLICT DO NOTHING`, pendingID, targetID, relation); err != nil {
			return fmt.Errorf("failed to write %s edge: %w", relation, err)
		}
		if _, err := tx.ExecContext(ctx, `
			WITH RECURSIVE affected(id) AS (
				SELECT source_id FROM lemn_edges WHERE target_id = $1 AND relationship = 'depends_on'
				UNION
				SELECT e.source_id FROM lemn_edges e JOIN affected a ON e.target_id = a.id WHERE e.relationship = 'depends_on'
			)
			UPDATE lemn_memories SET state = 'NEEDS_REVALIDATION'
			WHERE state = 'AUTHORITATIVE' AND id IN (SELECT id FROM affected)`, targetID); err != nil {
			return fmt.Errorf("failed to flag dependent memories for revalidation: %w", err)
		}
	}
	updatedProvenance, err := json.Marshal(prov)
	if err != nil {
		return fmt.Errorf("failed to encode reviewed provenance: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE lemn_memories SET provenance = $1 WHERE id = $2`, updatedProvenance, pendingID); err != nil {
		return fmt.Errorf("failed to save reviewed provenance: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `UPDATE lemn_memories SET state = 'AUTHORITATIVE' WHERE id = $1`, pendingID); err != nil {
		return fmt.Errorf("failed to promote memory #%d: %w", pendingID, err)
	}

	return tx.Commit()
}

func jsonInt(value interface{}) int {
	data, err := json.Marshal(value)
	if err != nil {
		return 0
	}
	var id int
	if json.Unmarshal(data, &id) != nil {
		return 0
	}
	return id
}

func jsonIntSlice(value interface{}) []int {
	data, err := json.Marshal(value)
	if err != nil {
		return nil
	}
	var ids []int
	if json.Unmarshal(data, &ids) != nil {
		return nil
	}
	return ids
}
