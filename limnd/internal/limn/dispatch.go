package limn

import (
	"context"
	"database/sql"
	"fmt"
)

func RouteExtraction(ctx context.Context, db *sql.DB, t TurnPayload, ext ModelExtraction) (int, error) {
	if !ext.MemoryWorthy {
		return 0, nil
	}

	embedding, err := getEmbedding(ctx, ext.Summary)
	if err != nil {
		return 0, fmt.Errorf("failed to embed extraction summary: %w", err)
	}

	mem := MemoryNode{
		Confidence: ext.Confidence,
		Category:   ext.Type,
		Summary:    ext.Summary,
		Rationale:  t.AssistantResponse,
		Embedding:  embedding,
		Provenance: map[string]interface{}{
			"source_turn_id": t.ID,
			"user_message":   t.UserMessage,
		},
	}

	intent := DetectCorrectionIntent(t.UserMessage)
	if intent.IsExplicitOverride {
		return ResolveTargetAndRoute(ctx, db, t.ToolCalls, mem)
	}
	return PromoteStandardMemory(ctx, db, mem, t.ToolCalls)
}
