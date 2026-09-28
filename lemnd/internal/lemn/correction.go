package lemn

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
)

// Statement-vs-statement similarity runs higher than the retrieval case, but
// 0.82 was tuned for a different embedder; override per deployment.
func supersedeThreshold() float64 {
	if v := getenv("LEMN_SUPERSEDE_THRESHOLD", ""); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return correctionMatchThreshold
}

const correctionMatchThreshold = 0.82

// GlobalScope holds memories that apply to every project.
const GlobalScope = "global"

// NormalizeScope maps an unset project id onto the global scope.
func NormalizeScope(projectID string) string {
	if projectID == "" {
		return GlobalScope
	}
	return projectID
}

var correctionRegex = regexp.MustCompile(`(?i)\b(forget|replace|no longer|instead of|switched from|migrate from|supersede)\b`)
var negationRegex = regexp.MustCompile(`(?i)\b(don't|do not|won't|will not|never|shouldn't|cannot|can't)\s+(replace|forget|supersede|migrate)\b`)

func DetectCorrectionIntent(userMessage string) CorrectionIntent {
	if negationRegex.MatchString(userMessage) {
		return CorrectionIntent{IsExplicitOverride: false, UserMessage: userMessage}
	}
	return CorrectionIntent{
		IsExplicitOverride: correctionRegex.MatchString(userMessage),
		UserMessage:        userMessage,
	}
}

// findSimilarAuthoritative searches only within the candidate's own scope. A
// project memory must never supersede a global one (or another project's),
// since that would silently retire a fact the other scope still relies on.
func findSimilarAuthoritative(ctx context.Context, db *sql.DB, embedding []float32, threshold float64, scope string) ([]MatchTarget, error) {
	embeddingJSON, _ := json.Marshal(embedding)
	rows, err := db.QueryContext(ctx, `
		SELECT id, summary, 1 - (embedding <=> $1::vector) as similarity
		FROM lemn_memories
		WHERE state = 'AUTHORITATIVE'
		  AND project_id = $3
		  AND 1 - (embedding <=> $1::vector) > $2
		ORDER BY similarity DESC LIMIT 5;`,
		string(embeddingJSON), threshold, NormalizeScope(scope))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var matches []MatchTarget
	for rows.Next() {
		var m MatchTarget
		if err := rows.Scan(&m.ID, &m.Summary, &m.Similarity); err == nil {
			matches = append(matches, m)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return matches, nil
}

func ResolveTargetAndRoute(ctx context.Context, db *sql.DB, toolCalls []ToolCallEvidence, newMem MemoryNode) (int, error) {
	candidateMatches, err := findSimilarAuthoritative(ctx, db, newMem.Embedding, supersedeThreshold(), newMem.ProjectID)
	if err != nil {
		return 0, fmt.Errorf("target search failed: %w", err)
	}

	if newMem.Provenance == nil {
		newMem.Provenance = make(map[string]interface{})
	}
	newMem.Provenance["candidate_targets"] = candidateMatches

	relevant, evidenceSource, err := hasRelevantEvidence(ctx, newMem, toolCalls)
	if err != nil {
		return 0, fmt.Errorf("evidence check failed: %w", err)
	}
	if relevant {
		newMem.Provenance["evidence_source"] = evidenceSource
	}

	if relevant && len(candidateMatches) == 1 {
		return SupersedeMemory(ctx, db, candidateMatches[0].ID, newMem)
	}

	newMem.State = "PENDING_CONFIRMATION"
	return insertMemory(ctx, db, newMem)
}
