package lemn

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
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
var temporaryPreferenceRegex = regexp.MustCompile(`(?i)\b(this answer only|for this answer|this task only|for this task only|just this time)\b`)

// IsTransientInstruction rejects preferences explicitly limited to one reply or task.
func IsTransientInstruction(userMessage string) bool {
	return temporaryPreferenceRegex.MatchString(userMessage)
}

// DetectExplicitGlobalPreference only overrides the learned scope classifier
// when the user explicitly combines durable-preference and cross-project cues.
func DetectExplicitGlobalPreference(userMessage string) bool {
	if IsTransientInstruction(userMessage) {
		return false
	}
	lower := strings.ToLower(userMessage)
	durable := strings.Contains(lower, "prefer") || strings.Contains(lower, "preference") ||
		strings.Contains(lower, "for future") || strings.Contains(lower, "i like")
	crossProject := strings.Contains(lower, "across all projects") || strings.Contains(lower, "across my projects") ||
		strings.Contains(lower, "across every project") || strings.Contains(lower, "in every project") ||
		strings.Contains(lower, "for every project") || strings.Contains(lower, "any project") ||
		strings.Contains(lower, "any codebase") || strings.Contains(lower, "no matter which project") ||
		strings.Contains(lower, "across your codebases") || strings.Contains(lower, "in all projects")
	return durable && crossProject
}

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

// FindSimilarAuthoritative embeds a newly extracted summary and searches its scope.
func FindSimilarAuthoritative(ctx context.Context, db *sql.DB, summary, scope string) ([]MatchTarget, []float32, error) {
	embedding, err := getEmbedding(ctx, summary)
	if err != nil {
		return nil, nil, fmt.Errorf("embed relation query: %w", err)
	}
	matches, err := findSimilarAuthoritative(ctx, db, embedding, supersedeThreshold(), scope)
	if err != nil {
		return nil, nil, fmt.Errorf("search relation candidates: %w", err)
	}
	return matches, embedding, nil
}

func ListDependencyCandidates(ctx context.Context, db *sql.DB, scope string, limit int) ([]DependencyCandidate, error) {
	if limit <= 0 {
		return nil, nil
	}
	scope = NormalizeScope(scope)
	rows, err := db.QueryContext(ctx, `
		SELECT id, project_id, summary
		FROM lemn_memories
		WHERE state = 'AUTHORITATIVE'
		  AND (project_id = $1 OR ($1 <> 'global' AND project_id = 'global'))
		ORDER BY created_at DESC, id DESC
		LIMIT $2`, scope, limit)
	if err != nil {
		return nil, fmt.Errorf("list dependency candidates: %w", err)
	}
	defer rows.Close()

	var candidates []DependencyCandidate
	for rows.Next() {
		var candidate DependencyCandidate
		if err := rows.Scan(&candidate.ID, &candidate.ProjectID, &candidate.Summary); err != nil {
			return nil, fmt.Errorf("scan dependency candidate: %w", err)
		}
		candidates = append(candidates, candidate)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate dependency candidates: %w", err)
	}
	return candidates, nil
}

func ListRelationCandidates(ctx context.Context, db *sql.DB, scope string, limit int) ([]DependencyCandidate, error) {
	if limit <= 0 {
		return nil, nil
	}
	scope = NormalizeScope(scope)
	rows, err := db.QueryContext(ctx, `
		SELECT id, project_id, summary
		FROM lemn_memories
		WHERE state = 'AUTHORITATIVE' AND project_id = $1
		ORDER BY created_at DESC, id DESC
		LIMIT $2`, scope, limit)
	if err != nil {
		return nil, fmt.Errorf("list relation candidates: %w", err)
	}
	defer rows.Close()

	var candidates []DependencyCandidate
	for rows.Next() {
		var candidate DependencyCandidate
		if err := rows.Scan(&candidate.ID, &candidate.ProjectID, &candidate.Summary); err != nil {
			return nil, fmt.Errorf("scan relation candidate: %w", err)
		}
		candidates = append(candidates, candidate)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate relation candidates: %w", err)
	}
	return candidates, nil
}
