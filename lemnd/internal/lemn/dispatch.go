package lemn

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"regexp"
	"strconv"
)

const dependencyCandidateLimit = 40

var metaSummaryRegex = regexp.MustCompile(`(?i)^\s*(the\s+)?assistant\s|^\s*confirming\s+memory\b`)

// IsMetaSummary reports summaries that narrate the conversation instead of stating a fact.
func IsMetaSummary(summary string) bool {
	return metaSummaryRegex.MatchString(summary)
}

// resolveRelation keeps a supersedes/contradicts proposal only when it names a
// valid target; without one it can never be applied and would only stall review.
func resolveRelation(prov map[string]interface{}, relation string, targetID int, targetValid bool) string {
	delete(prov, "proposed_relation")
	delete(prov, "proposed_target_id")
	if relation != "supersedes" && relation != "contradicts" {
		return "independent"
	}
	if targetID <= 0 || !targetValid {
		prov["downgraded_relation"] = relation
		return "independent"
	}
	prov["proposed_relation"] = relation
	prov["proposed_target_id"] = targetID
	return relation
}

func RouteExtraction(ctx context.Context, db *sql.DB, t TurnPayload, ext ModelExtraction) (int, error) {
	if !ext.MemoryWorthy {
		return 0, nil
	}

	embedding := ext.SummaryEmbedding
	if len(embedding) == 0 {
		var err error
		embedding, err = getEmbedding(ctx, ext.Summary)
		if err != nil {
			return 0, fmt.Errorf("failed to embed extraction summary: %w", err)
		}
	}

	scope := NormalizeScope(t.ProjectID)
	if ext.GlobalScoped {
		scope = GlobalScope
	}

	mem := MemoryNode{
		ProjectID:  scope,
		Confidence: ext.Confidence,
		Category:   ext.Type,
		Summary:    ext.Summary,
		Rationale:  t.AssistantResponse,
		Embedding:  embedding,
		Provenance: map[string]interface{}{
			"source_turn_id":        t.ID,
			"user_message":          t.UserMessage,
			"kernel_schema_version": 2,
		},
	}

	targets := ext.RelationCandidates
	if targets == nil {
		var err error
		targets, err = findSimilarAuthoritative(ctx, db, embedding, supersedeThreshold(), scope)
		if err != nil {
			return 0, fmt.Errorf("target search failed: %w", err)
		}
	}
	mem.Provenance["candidate_targets"] = targets

	evidence, err := measureEvidence(ctx, mem, t.ToolCalls)
	if err != nil {
		return 0, fmt.Errorf("evidence check failed: %w", err)
	}
	relevant := evidence.Relevant
	if relevant {
		mem.Provenance["evidence_source"] = evidence.Tool
	}
	signals := map[string]interface{}{
		"gate_probability": ext.GateProbability,
		"tool_calls":       len(t.ToolCalls),
	}
	if evidence.Tool != "" {
		signals["evidence_similarity"] = evidence.Similarity
		signals["evidence_tool"] = evidence.Tool
	}
	corroborating, err := findCorroborating(ctx, db, embedding, scope)
	if err != nil {
		return 0, fmt.Errorf("corroboration lookup failed: %w", err)
	}
	signals["corroborating_ids"] = corroborating
	mem.Provenance["signals"] = signals

	candidates, err := ListDependencyCandidates(ctx, db, scope, dependencyCandidateLimit)
	if err != nil {
		return 0, fmt.Errorf("dependency candidate lookup failed: %w", err)
	}
	validIDs := make(map[int]struct{}, len(candidates))
	for _, candidate := range candidates {
		validIDs[candidate.ID] = struct{}{}
	}
	allowedDependencyIDs := make(map[int]struct{}, len(ext.DependencyCandidateIDs))
	for _, id := range ext.DependencyCandidateIDs {
		allowedDependencyIDs[id] = struct{}{}
	}
	for id := range validIDs {
		if _, allowed := allowedDependencyIDs[id]; !allowed {
			delete(validIDs, id)
		}
	}
	relationTargetIDs := make(map[int]struct{}, len(targets))
	for _, candidate := range targets {
		if candidate.Similarity >= supersedeThreshold() {
			relationTargetIDs[candidate.ID] = struct{}{}
		}
	}
	dependencies := approvedDependencyIDs(ext.DependsOn, validIDs)
	mem.Provenance["depends_on"] = dependencies

	_, targetValid := relationTargetIDs[ext.TargetID]
	relation := resolveRelation(mem.Provenance, ext.Relation, ext.TargetID, targetValid)
	if relation != "independent" {
		targetSimilarity, err := similarityTo(ctx, db, embedding, ext.TargetID)
		if err != nil {
			return 0, fmt.Errorf("target similarity failed: %w", err)
		}
		signals["target_similarity"] = targetSimilarity
	}

	preAutoState := extractionState(DetectCorrectionIntent(t.UserMessage).IsExplicitOverride, relation, relevant)
	mem.State = autoPromoteState(preAutoState, mem.Confidence)
	autoPromote := mem.State == "AUTHORITATIVE"
	if autoPromote {
		mem.Provenance["auto_promoted"] = true
		if preAutoState == "OBSERVED" {
			mem.Provenance["auto_promote_reason"] = "confidence"
		}
		mem.State = preAutoState
	}

	autoConfirm := shouldAutoConfirm(relation, mem.Confidence, relevant, mem.Provenance)
	if autoConfirm {
		stampAutoSupersede(mem)
	}

	id, err := insertMemory(ctx, db, mem)
	if err != nil {
		return 0, err
	}
	if autoConfirm || autoPromote {
		if err := ConfirmPendingMemory(ctx, db, id); err != nil {
			// Keep the original reviewable state; do not retry the job and insert a duplicate.
			log.Printf("[AutoPromotion] memory #%d remains %s after confirmation failed: %v", id, preAutoState, err)
		}
	}
	return id, nil
}

// promoteThreshold is the confidence at or above which an unbacked (OBSERVED)
// claim is auto-promoted instead of waiting for human review. Claims that
// mutate the graph (supersedes, contradicts, corrections) are never
// auto-promoted, regardless of confidence.
func promoteThreshold() float64 {
	if v := getenv("LEMN_PROMOTE_THRESHOLD", ""); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return 0.9
}

// autoPromoteState promotes claims that need no judgment call so the review
// queue stays small. Evidence-backed independent claims (CANDIDATE) are always
// promoted; unbacked claims (OBSERVED) are promoted only when the extraction
// model's own confidence clears promoteThreshold. Everything that mutates the
// graph (supersedes, contradicts, corrections) stays human-gated here; the
// narrow exception is shouldAutoConfirm, which confirms an evidence-backed,
// high-confidence relation proposal right after insert.
func autoPromoteState(state string, confidence float64) string {
	if state == "CANDIDATE" {
		return "AUTHORITATIVE"
	}
	if state == "OBSERVED" && confidence >= promoteThreshold() {
		return "AUTHORITATIVE"
	}
	return state
}

// autoSupersedeThreshold is the confidence at or above which a
// supersedes/contradicts proposal backed by relevant tool evidence is
// confirmed automatically, applying the graph mutation without human review.
// Deliberately stricter than promoteThreshold: this path destroys a
// previously-authoritative memory, so the bar is higher than the one for
// promoting a leaf.
func autoSupersedeThreshold() float64 {
	if v := getenv("LEMN_AUTO_SUPERSEDE_THRESHOLD", ""); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return 0.95
}

// shouldAutoConfirm reports whether a relation proposal is safe to confirm
// without a human: it must be a destructive relation (supersedes/contradicts)
// with a valid target, backed by relevant tool evidence, and the extraction
// model's own confidence must clear autoSupersedeThreshold. Unbacked
// proposals, whatever their confidence, stay human-gated.
func shouldAutoConfirm(relation string, confidence float64, hasEvidence bool, provenance map[string]interface{}) bool {
	if relation != "supersedes" && relation != "contradicts" {
		return false
	}
	if !hasEvidence {
		return false
	}
	if confidence < autoSupersedeThreshold() {
		return false
	}
	return jsonInt(provenance["proposed_target_id"]) > 0
}

// stampAutoSupersede records in provenance that the auto-confirm path will be
// attempted, and drops any dependency on the relation target — once the
// target is flipped it is no longer authoritative, and ConfirmPendingMemory
// rejects dependencies on non-authoritative memories.
func stampAutoSupersede(mem MemoryNode) {
	mem.Provenance["auto_supersede"] = true
	mem.Provenance["auto_supersede_reason"] = "evidence+confidence"
	if deps := jsonIntSlice(mem.Provenance["depends_on"]); len(deps) > 0 {
		targetID := jsonInt(mem.Provenance["proposed_target_id"])
		filtered := deps[:0]
		for _, dep := range deps {
			if dep != targetID {
				filtered = append(filtered, dep)
			}
		}
		mem.Provenance["depends_on"] = filtered
	}
}

func approvedDependencyIDs(proposed []int, validIDs map[int]struct{}) []int {
	dependencies := make([]int, 0, len(proposed))
	seen := make(map[int]struct{}, len(proposed))
	for _, id := range proposed {
		if _, ok := validIDs[id]; !ok || id <= 0 {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		dependencies = append(dependencies, id)
	}
	return dependencies
}

func extractionState(explicitCorrection bool, relation string, hasRelevantEvidence bool) string {
	if explicitCorrection || (relation != "independent" && relation != "") {
		return "PENDING_CONFIRMATION"
	}
	if hasRelevantEvidence {
		return "CANDIDATE"
	}
	return "OBSERVED"
}

type SweepAction struct {
	ID      int
	Action  string // reject, auto-confirm, promote, requeue, keep
	Reason  string
	Summary string
	Err     error
}

// SweepPending re-applies the current auto-review rules to memories queued
// under older logic. With apply=false it only reports what it would do.
func SweepPending(ctx context.Context, db *sql.DB, apply bool) ([]SweepAction, error) {
	type pendingRow struct {
		id                    int
		state, scope, summary string
		confidence            float64
		prov                  map[string]interface{}
	}
	rows, err := db.QueryContext(ctx, `
		SELECT id, state, project_id, summary, confidence, provenance
		FROM lemn_memories
		WHERE state IN ('OBSERVED', 'CANDIDATE', 'PENDING_CONFIRMATION', 'NEEDS_REVALIDATION')
		ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("list reviewable memories: %w", err)
	}
	var pending []pendingRow
	for rows.Next() {
		var r pendingRow
		var provJSON []byte
		if err := rows.Scan(&r.id, &r.state, &r.scope, &r.summary, &r.confidence, &provJSON); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan reviewable memory: %w", err)
		}
		if len(provJSON) > 0 {
			_ = json.Unmarshal(provJSON, &r.prov)
		}
		if r.prov == nil {
			r.prov = make(map[string]interface{})
		}
		pending = append(pending, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate reviewable memories: %w", err)
	}

	var actions []SweepAction
	for _, r := range pending {
		a := SweepAction{ID: r.id, Summary: r.summary}
		if r.state == "NEEDS_REVALIDATION" {
			a.Action, a.Reason = "keep", "requires fresh evidence or explicit user confirmation"
			actions = append(actions, a)
			continue
		}
		if IsMetaSummary(r.summary) {
			a.Action, a.Reason = "reject", "narrates the conversation"
			if apply {
				r.prov["reject_reason"] = "meta_summary"
				a.Err = setStateAndProvenance(ctx, db, r.id, "REJECTED", r.prov)
			}
			actions = append(actions, a)
			continue
		}

		proposed, _ := r.prov["proposed_relation"].(string)
		targetID := jsonInt(r.prov["proposed_target_id"])
		targetValid := false
		if targetID > 0 {
			if targetValid, err = relationTargetValid(ctx, db, targetID, r.scope); err != nil {
				return actions, err
			}
		}
		relation := resolveRelation(r.prov, proposed, targetID, targetValid)
		hasEvidence := r.prov["evidence_source"] != nil
		userMessage, _ := r.prov["user_message"].(string)
		explicit := DetectCorrectionIntent(userMessage).IsExplicitOverride
		state := autoPromoteState(extractionState(explicit, relation, hasEvidence), r.confidence)

		switch {
		case shouldAutoConfirm(relation, r.confidence, hasEvidence, r.prov):
			a.Action, a.Reason = "auto-confirm", fmt.Sprintf("%s #%d, backed by %v", relation, targetID, r.prov["evidence_source"])
			if apply {
				if a.Err = filterStaleDependencies(ctx, db, r.scope, r.prov); a.Err == nil {
					stampAutoSupersede(MemoryNode{Provenance: r.prov})
					a.Err = persistAndConfirmMemory(ctx, db, r.id, r.state, r.prov)
				}
			}
		case state == "AUTHORITATIVE":
			reason := "confidence"
			if hasEvidence {
				reason = "evidence"
			}
			a.Action, a.Reason = "promote", reason
			if proposed != "" && relation == "independent" {
				a.Reason += fmt.Sprintf("; %s dropped, target #%d invalid", proposed, targetID)
			}
			if apply {
				if a.Err = filterStaleDependencies(ctx, db, r.scope, r.prov); a.Err == nil {
					r.prov["auto_promoted"] = true
					r.prov["auto_promote_reason"] = reason
					a.Err = persistAndConfirmMemory(ctx, db, r.id, r.state, r.prov)
				}
			}
		case state != r.state:
			a.Action, a.Reason = "requeue", fmt.Sprintf("%s -> %s", r.state, state)
			if apply {
				a.Err = setStateAndProvenance(ctx, db, r.id, state, r.prov)
			}
		default:
			a.Action = "keep"
			switch {
			case relation != "independent" && !hasEvidence:
				a.Reason = fmt.Sprintf("%s #%d without tool evidence", relation, targetID)
			case relation != "independent":
				a.Reason = fmt.Sprintf("%s #%d below auto-supersede threshold", relation, targetID)
			case proposed != "" && relation == "independent":
				a.Reason = fmt.Sprintf("%s dropped, target #%d invalid; confidence below promote threshold", proposed, targetID)
			case explicit:
				a.Reason = "explicit correction in user message"
			default:
				a.Reason = "confidence below promote threshold"
			}
		}
		actions = append(actions, a)
	}
	return actions, nil
}

// corroborationThreshold marks a near-restatement of an existing claim; above
// supersedeThreshold because repeating a fact is closer than replacing it.
const corroborationThreshold = 0.90

// findCorroborating returns earlier non-rejected memories in the same scope
// that state nearly the same claim.
func findCorroborating(ctx context.Context, db *sql.DB, embedding []float32, scope string) ([]int, error) {
	embeddingJSON, _ := json.Marshal(embedding)
	rows, err := db.QueryContext(ctx, `
		SELECT id FROM lemn_memories
		WHERE project_id = $2 AND state <> 'REJECTED'
		  AND 1 - (embedding <=> $1::vector) >= $3
		ORDER BY id LIMIT 10`, string(embeddingJSON), scope, corroborationThreshold)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := []int{}
	for rows.Next() {
		var id int
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func similarityTo(ctx context.Context, db *sql.DB, embedding []float32, id int) (float64, error) {
	embeddingJSON, _ := json.Marshal(embedding)
	var s float64
	err := db.QueryRowContext(ctx, `SELECT 1 - (embedding <=> $1::vector) FROM lemn_memories WHERE id = $2`, string(embeddingJSON), id).Scan(&s)
	return s, err
}

func relationTargetValid(ctx context.Context, db *sql.DB, targetID int, scope string) (bool, error) {
	var n int
	err := db.QueryRowContext(ctx, `
		SELECT count(*) FROM lemn_memories
		WHERE id = $1 AND state = 'AUTHORITATIVE' AND project_id = $2`, targetID, scope).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("check relation target #%d: %w", targetID, err)
	}
	return n > 0, nil
}

func setStateAndProvenance(ctx context.Context, db *sql.DB, id int, state string, prov map[string]interface{}) error {
	provJSON, err := json.Marshal(prov)
	if err != nil {
		return fmt.Errorf("encode provenance for #%d: %w", id, err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE lemn_memories SET state = $1, provenance = $2 WHERE id = $3`, state, provJSON, id); err != nil {
		return fmt.Errorf("update memory #%d: %w", id, err)
	}
	return nil
}

func persistAndConfirmMemory(ctx context.Context, db *sql.DB, id int, state string, prov map[string]interface{}) error {
	if err := setStateAndProvenance(ctx, db, id, state, prov); err != nil {
		return err
	}
	return ConfirmPendingMemory(ctx, db, id)
}

// dependencyVisible reports whether a memory in depScope is visible from a
// memory in scope: same scope, or global from a project scope.
func dependencyVisible(depScope, scope string) bool {
	return depScope == scope || (scope != GlobalScope && depScope == GlobalScope)
}

// filterStaleDependencies drops depends_on ids that are no longer authoritative
// and visible in the memory's scope. A dependency flipped after the memory was
// queued (by another confirmation's cascade, or long ago) would otherwise make
// ConfirmPendingMemory fail; the memory was not authoritative when the
// dependency flipped, so nothing relied on the edge. Dropped ids are recorded
// in provenance for audit.
func filterStaleDependencies(ctx context.Context, db *sql.DB, scope string, prov map[string]interface{}) error {
	deps := jsonIntSlice(prov["depends_on"])
	if len(deps) == 0 {
		return nil
	}
	kept := make([]int, 0, len(deps))
	var dropped []int
	for _, id := range deps {
		var state, depScope string
		if id <= 0 {
			dropped = append(dropped, id)
			continue
		}
		err := db.QueryRowContext(ctx, `SELECT state, project_id FROM lemn_memories WHERE id = $1`, id).Scan(&state, &depScope)
		if errors.Is(err, sql.ErrNoRows) {
			dropped = append(dropped, id)
			continue
		}
		if err != nil {
			return fmt.Errorf("check dependency #%d: %w", id, err)
		}
		if state == "AUTHORITATIVE" && dependencyVisible(depScope, scope) {
			kept = append(kept, id)
		} else {
			dropped = append(dropped, id)
		}
	}
	prov["depends_on"] = kept
	if len(dropped) > 0 {
		prov["dropped_dependencies"] = dropped
	}
	return nil
}
