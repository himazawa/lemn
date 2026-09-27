package limn

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
)

func QueryAuthoritativeMemories(ctx context.Context, db *sql.DB, queryStr string, limit int) ([]RetrievedMemory, error) {
	if limit <= 0 {
		limit = 5
	}

	queryEmbedding, err := getEmbedding(ctx, queryStr)
	if err != nil {
		return nil, fmt.Errorf("failed to embed retrieval query: %w", err)
	}

	embeddingJSON, _ := json.Marshal(queryEmbedding)

	query := `
		SELECT id, category, summary, 1 - (embedding <=> $1::vector) as similarity
		FROM limn_memories
		WHERE state = 'AUTHORITATIVE'
		  AND 1 - (embedding <=> $1::vector) > 0.75
		ORDER BY similarity DESC
		LIMIT $2;`

	rows, err := db.QueryContext(ctx, query, string(embeddingJSON), limit)
	if err != nil {
		return nil, fmt.Errorf("vector similarity query failed: %w", err)
	}
	defer rows.Close()

	var memories []RetrievedMemory
	for rows.Next() {
		var m RetrievedMemory
		if err := rows.Scan(&m.ID, &m.Category, &m.Summary, &m.Similarity); err == nil {
			memories = append(memories, m)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("vector similarity iteration failed: %w", err)
	}

	return memories, nil
}
