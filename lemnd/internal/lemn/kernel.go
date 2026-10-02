package lemn

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

type ConfirmationDecision struct {
	Relation            string
	TargetID            int
	DependsOn           []int
	ReplaceDependencies bool
}

type RevalidationDecision struct {
	Evidence            string
	EvidenceSource      string
	EvidenceObservedAt  time.Time
	UserConfirmed       bool
	Summary             string
	DependsOn           []int
	ReplaceDependencies bool
}

const evidenceRelevanceThreshold = 0.60
const defaultRevalidationEvidenceMaxAge = 30 * 24 * time.Hour

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
	case "NEEDS_REVALIDATION":
		return fmt.Errorf("memory #%d requires explicit revalidation before confirmation", pendingID)
	case "OBSERVED", "CANDIDATE", "PENDING", "PENDING_CONFIRMATION":
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
		revalidationReason := "dependency_superseded"
		if relation == "contradicts" {
			revalidationReason = "dependency_contradicted"
		}
		if _, err := tx.ExecContext(ctx, `
			WITH RECURSIVE affected(id) AS (
				SELECT source_id FROM lemn_edges WHERE target_id = $1 AND relationship = 'depends_on'
				UNION
				SELECT e.source_id FROM lemn_edges e JOIN affected a ON e.target_id = a.id WHERE e.relationship = 'depends_on'
			)
			UPDATE lemn_memories
			SET state = 'NEEDS_REVALIDATION',
			    provenance = COALESCE(provenance, '{}'::jsonb) || jsonb_build_object(
			        'revalidation_reason', $2::text,
			        'invalidated_by_memory_id', $3::int,
			        'invalidated_dependency_id', $1,
			        'invalidated_at', $4::text
			    )
			WHERE state = 'AUTHORITATIVE' AND id IN (SELECT id FROM affected)`, targetID, revalidationReason, pendingID, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
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

// RevalidateMemory restores a quarantined memory only after fresh evidence or
// explicit user confirmation, while preserving its established relations.
func RevalidateMemory(ctx context.Context, db *sql.DB, memoryID int, decision RevalidationDecision) error {
	evidence := strings.TrimSpace(decision.Evidence)
	evidenceSource := strings.TrimSpace(decision.EvidenceSource)
	maxEvidenceAge, err := revalidationEvidenceMaxAge()
	if err != nil {
		return err
	}
	if err := validateRevalidationDecision(decision, time.Now(), maxEvidenceAge); err != nil {
		return err
	}

	newSummary := strings.TrimSpace(decision.Summary)
	var embeddingJSON []byte
	if newSummary != "" {
		embedding, err := getEmbedding(ctx, newSummary)
		if err != nil {
			return fmt.Errorf("failed to embed revised memory summary: %w", err)
		}
		embeddingJSON, err = json.Marshal(embedding)
		if err != nil {
			return fmt.Errorf("failed to encode revised memory embedding: %w", err)
		}
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to begin revalidation: %w", err)
	}
	defer tx.Rollback()

	var state, projectID, oldSummary string
	var provenanceJSON []byte
	if err := tx.QueryRowContext(ctx, `
		SELECT state, project_id, summary, provenance
		FROM lemn_memories WHERE id = $1 FOR UPDATE`, memoryID,
	).Scan(&state, &projectID, &oldSummary, &provenanceJSON); err != nil {
		return fmt.Errorf("failed to fetch memory #%d for revalidation: %w", memoryID, err)
	}
	if state != "NEEDS_REVALIDATION" {
		return fmt.Errorf("memory #%d is not awaiting revalidation (state=%s)", memoryID, state)
	}
	var provenance map[string]interface{}
	if len(provenanceJSON) > 0 {
		if err := json.Unmarshal(provenanceJSON, &provenance); err != nil {
			return fmt.Errorf("failed to parse provenance for #%d: %w", memoryID, err)
		}
	}
	if provenance == nil {
		provenance = make(map[string]interface{})
	}
	var invalidatedAt time.Time
	if rawInvalidatedAt, ok := provenance["invalidated_at"].(string); ok && rawInvalidatedAt != "" {
		invalidatedAt, err = time.Parse(time.RFC3339Nano, rawInvalidatedAt)
		if err != nil {
			return fmt.Errorf("memory #%d has an invalid invalidation timestamp; use explicit user confirmation", memoryID)
		}
	}
	if evidence != "" {
		if invalidatedAt.IsZero() {
			return fmt.Errorf("memory #%d has no invalidation timestamp; use explicit user confirmation", memoryID)
		}
		if decision.EvidenceObservedAt.Before(invalidatedAt) {
			return fmt.Errorf("evidence for memory #%d predates its invalidation", memoryID)
		}
	}

	dependencies := append([]int(nil), decision.DependsOn...)
	if !decision.ReplaceDependencies {
		rows, err := tx.QueryContext(ctx, `
			SELECT target_id FROM lemn_edges
			WHERE source_id = $1 AND relationship = 'depends_on'
			ORDER BY target_id`, memoryID)
		if err != nil {
			return fmt.Errorf("failed to load dependencies for memory #%d: %w", memoryID, err)
		}
		for rows.Next() {
			var dependencyID int
			if err := rows.Scan(&dependencyID); err != nil {
				rows.Close()
				return fmt.Errorf("failed to read dependencies for memory #%d: %w", memoryID, err)
			}
			dependencies = append(dependencies, dependencyID)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return fmt.Errorf("failed to iterate dependencies for memory #%d: %w", memoryID, err)
		}
		if err := rows.Close(); err != nil {
			return fmt.Errorf("failed to close dependencies for memory #%d: %w", memoryID, err)
		}
	} else {
		dependencies = uniqueSortedIDs(dependencies)
		if _, err := tx.ExecContext(ctx, `DELETE FROM lemn_edges WHERE source_id = $1 AND relationship = 'depends_on'`, memoryID); err != nil {
			return fmt.Errorf("failed to replace dependencies for memory #%d: %w", memoryID, err)
		}
	}

	dependencies = uniqueSortedIDs(dependencies)
	for _, dependencyID := range dependencies {
		if dependencyID <= 0 || dependencyID == memoryID {
			return fmt.Errorf("invalid dependency id %d for memory #%d", dependencyID, memoryID)
		}
		var dependencyState, dependencyScope string
		if err := tx.QueryRowContext(ctx, `
			SELECT state, project_id FROM lemn_memories WHERE id = $1 FOR UPDATE`, dependencyID,
		).Scan(&dependencyState, &dependencyScope); err != nil {
			return fmt.Errorf("failed to lock dependency #%d: %w", dependencyID, err)
		}
		if dependencyState != "AUTHORITATIVE" || !dependencyVisible(dependencyScope, projectID) {
			return fmt.Errorf("dependency #%d is not authoritative and visible in scope %q", dependencyID, projectID)
		}
		if decision.ReplaceDependencies {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO lemn_edges (source_id, target_id, relationship)
				VALUES ($1, $2, 'depends_on') ON CONFLICT DO NOTHING`, memoryID, dependencyID); err != nil {
				return fmt.Errorf("failed to add dependency edge to #%d: %w", dependencyID, err)
			}
		}
	}

	entry := map[string]interface{}{
		"revalidated_at": time.Now().UTC().Format(time.RFC3339Nano),
		"user_confirmed": decision.UserConfirmed,
	}
	if evidence != "" {
		entry["evidence_note"] = evidence
		digest := sha256.Sum256([]byte(evidence))
		entry["evidence_note_sha256"] = hex.EncodeToString(digest[:])
		entry["evidence_source"] = evidenceSource
		entry["evidence_observed_at"] = decision.EvidenceObservedAt.UTC().Format(time.RFC3339Nano)
	}
	if reason, ok := provenance["revalidation_reason"]; ok {
		entry["reason"] = reason
	}
	if invalidatedBy, ok := provenance["invalidated_by_memory_id"]; ok {
		entry["invalidated_by_memory_id"] = invalidatedBy
	}
	if invalidatedDependency, ok := provenance["invalidated_dependency_id"]; ok {
		entry["invalidated_dependency_id"] = invalidatedDependency
	}
	if invalidatedAt, ok := provenance["invalidated_at"]; ok {
		entry["invalidated_at"] = invalidatedAt
	}
	if newSummary != "" && newSummary != oldSummary {
		entry["previous_summary"] = oldSummary
		entry["revised_summary"] = newSummary
	}
	history, _ := provenance["revalidation_history"].([]interface{})
	provenance["revalidation_history"] = append(history, entry)
	provenance["depends_on"] = dependencies
	delete(provenance, "revalidation_reason")
	delete(provenance, "invalidated_by_memory_id")
	delete(provenance, "invalidated_dependency_id")
	delete(provenance, "invalidated_at")
	updatedProvenance, err := json.Marshal(provenance)
	if err != nil {
		return fmt.Errorf("failed to encode revalidation provenance for #%d: %w", memoryID, err)
	}

	if len(embeddingJSON) > 0 {
		_, err = tx.ExecContext(ctx, `
			UPDATE lemn_memories SET state = 'AUTHORITATIVE', summary = $1, embedding = $2::vector, provenance = $3
			WHERE id = $4`, newSummary, string(embeddingJSON), updatedProvenance, memoryID)
	} else {
		_, err = tx.ExecContext(ctx, `
			UPDATE lemn_memories SET state = 'AUTHORITATIVE', provenance = $1 WHERE id = $2`, updatedProvenance, memoryID)
	}
	if err != nil {
		return fmt.Errorf("failed to restore revalidated memory #%d: %w", memoryID, err)
	}
	return tx.Commit()
}

func revalidationEvidenceMaxAge() (time.Duration, error) {
	value := getenv("LEMN_REVALIDATION_MAX_EVIDENCE_AGE", defaultRevalidationEvidenceMaxAge.String())
	maxAge, err := time.ParseDuration(value)
	if err != nil || maxAge <= 0 {
		return 0, fmt.Errorf("LEMN_REVALIDATION_MAX_EVIDENCE_AGE must be a positive duration, got %q", value)
	}
	return maxAge, nil
}

func validateRevalidationDecision(decision RevalidationDecision, now time.Time, maxAge time.Duration) error {
	evidence := strings.TrimSpace(decision.Evidence)
	source := strings.TrimSpace(decision.EvidenceSource)
	if evidence == "" {
		if source != "" || !decision.EvidenceObservedAt.IsZero() {
			return fmt.Errorf("evidence source and observation time require an evidence note")
		}
		if !decision.UserConfirmed {
			return fmt.Errorf("revalidation requires a sourced evidence note or explicit user confirmation")
		}
		return nil
	}
	if source == "" {
		return fmt.Errorf("an evidence source reference is required with an evidence note")
	}
	if decision.EvidenceObservedAt.IsZero() {
		return fmt.Errorf("an evidence observation time is required with an evidence note")
	}
	if decision.EvidenceObservedAt.After(now) {
		return fmt.Errorf("evidence observation time cannot be in the future")
	}
	if maxAge <= 0 {
		return fmt.Errorf("maximum evidence age must be positive")
	}
	if age := now.Sub(decision.EvidenceObservedAt); age > maxAge {
		return fmt.Errorf("evidence is older than the maximum age of %s; use explicit user confirmation", maxAge)
	}
	return nil
}

func uniqueSortedIDs(ids []int) []int {
	seen := make(map[int]struct{}, len(ids))
	unique := make([]int, 0, len(ids))
	for _, id := range ids {
		if _, exists := seen[id]; exists {
			continue
		}
		seen[id] = struct{}{}
		unique = append(unique, id)
	}
	sort.Ints(unique)
	return unique
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
