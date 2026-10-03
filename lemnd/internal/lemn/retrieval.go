package lemn

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"
)

// Calibrated for bge-m3: question-vs-statement similarity peaks near 0.55, so
// the old 0.75 constant matched nothing. Re-measure if you change embedders.
func retrievalThreshold() float64 {
	if v := getenv("LEMN_RETRIEVAL_THRESHOLD", ""); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return 0.45
}

// retrievalFallbackMargin is how far below the retrieval threshold the best
// match may fall and still trigger the fallback. A broad question like "what do
// you know about this project?" embeds a hair under the bar against narrow
// summaries (e.g. 0.446 vs a 0.45 threshold); without the fallback, /retrieve
// returns [] and the model answers "I have no memory". With it, a near miss
// still returns the top memories, flagged BelowThreshold so the prompt can
// present them as weaker context.
//
// Set to 0 to disable the fallback entirely (only exact-threshold matches).
// Set large (e.g. 1.0) to make it unconditional: whenever anything is
// AUTHORITATIVE in scope, the top-N is injected.
func retrievalFallbackMargin() float64 {
	if v := getenv("LEMN_RETRIEVAL_FALLBACK_MARGIN", ""); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return 0.10
}

// selectRetrievalResults applies the threshold and the near-miss fallback to a
// ranked (similarity DESC) list of scoped candidates. It is pure so the
// decision logic is unit-testable without a database or embedder.
//
//   - any candidate above the threshold is returned, up to limit.
//   - otherwise, if the best candidate is a near miss (within fallbackMargin
//     below the threshold), the top candidates are returned and flagged
//     BelowThreshold, so a broad question still gets something.
//   - otherwise nothing is returned: the query is genuinely unrelated, and
//     injecting stale memories would only fill the prompt with noise.
func selectRetrievalResults(candidates []RetrievedMemory, threshold, fallbackMargin float64, limit int) []RetrievedMemory {
	if limit <= 0 {
		limit = 5
	}

	above := make([]RetrievedMemory, 0, limit)
	for _, c := range candidates {
		if c.Similarity > threshold {
			above = append(above, c)
			if len(above) >= limit {
				break
			}
		}
	}
	if len(above) > 0 {
		return above
	}

	if len(candidates) == 0 {
		return []RetrievedMemory{}
	}

	// candidates is ranked DESC, so candidates[0] is the best (closest) match.
	if candidates[0].Similarity >= threshold-fallbackMargin {
		fallback := make([]RetrievedMemory, 0, limit)
		for _, c := range candidates {
			c.BelowThreshold = true
			fallback = append(fallback, c)
			if len(fallback) >= limit {
				break
			}
		}
		return fallback
	}

	return []RetrievedMemory{}
}

func QueryAuthoritativeMemories(ctx context.Context, db *sql.DB, queryStr string, projectID string, limit int) ([]RetrievedMemory, error) {
	if limit <= 0 {
		limit = 5
	}

	queryEmbedding, err := getEmbedding(ctx, queryStr)
	if err != nil {
		return nil, fmt.Errorf("failed to embed retrieval query: %w", err)
	}

	embeddingJSON, _ := json.Marshal(queryEmbedding)

	// Reads span the caller's project plus the global scope; writes never do.
	// The threshold is NOT applied in SQL: we fetch the top `limit` ranked
	// candidates and let selectRetrievalResults apply the threshold and the
	// near-miss fallback, so a single query serves both paths.
	query := `
		SELECT id, project_id, category, summary, 1 - (embedding <=> $1::vector) as similarity
		FROM lemn_memories
		WHERE state = 'AUTHORITATIVE'
		  AND project_id IN ($3, $4)
		ORDER BY similarity DESC
		LIMIT $2;`

	rows, err := db.QueryContext(ctx, query, string(embeddingJSON), limit, NormalizeScope(projectID), GlobalScope)
	if err != nil {
		return nil, fmt.Errorf("vector similarity query failed: %w", err)
	}
	defer rows.Close()

	candidates := []RetrievedMemory{}
	for rows.Next() {
		var m RetrievedMemory
		if err := rows.Scan(&m.ID, &m.ProjectID, &m.Category, &m.Summary, &m.Similarity); err != nil {
			return nil, fmt.Errorf("scan vector similarity candidate: %w", err)
		}
		candidates = append(candidates, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("vector similarity iteration failed: %w", err)
	}

	return selectRetrievalResults(candidates, retrievalThreshold(), retrievalFallbackMargin(), limit), nil
}
