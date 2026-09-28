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
	query := `
		SELECT id, project_id, category, summary, 1 - (embedding <=> $1::vector) as similarity
		FROM lemn_memories
		WHERE state = 'AUTHORITATIVE'
		  AND project_id IN ($3, $4)
		  AND 1 - (embedding <=> $1::vector) > $5
		ORDER BY similarity DESC
		LIMIT $2;`

	rows, err := db.QueryContext(ctx, query, string(embeddingJSON), limit, NormalizeScope(projectID), GlobalScope, retrievalThreshold())
	if err != nil {
		return nil, fmt.Errorf("vector similarity query failed: %w", err)
	}
	defer rows.Close()

	memories := []RetrievedMemory{}
	for rows.Next() {
		var m RetrievedMemory
		if err := rows.Scan(&m.ID, &m.ProjectID, &m.Category, &m.Summary, &m.Similarity); err == nil {
			memories = append(memories, m)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("vector similarity iteration failed: %w", err)
	}

	return memories, nil
}
