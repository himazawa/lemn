package lemn

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	_ "github.com/lib/pq"
)

func TestRouteExtractionAutoPromotionWritesDependencyEdges(t *testing.T) {
	dsn := os.Getenv("LEMN_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set LEMN_TEST_POSTGRES_DSN to an isolated database initialized from schema.sql")
	}

	embedding := make([]float32, 1024)
	embedding[0] = 1
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"data": []map[string]interface{}{{"embedding": embedding}},
		})
	}))
	defer server.Close()
	t.Setenv("LEMN_EMBEDDING_URL", server.URL)
	t.Setenv("LEMN_EMBEDDING_MODEL", "test-embedding")
	t.Setenv("LEMN_PROMOTE_THRESHOLD", "0.9")

	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("open test Postgres: %v", err)
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	projectID := fmt.Sprintf("dependency-edge-test-%d", time.Now().UnixNano())
	defer func() {
		_, _ = db.ExecContext(ctx, `DELETE FROM lemn_edges WHERE source_id IN (SELECT id FROM lemn_memories WHERE project_id = $1) OR target_id IN (SELECT id FROM lemn_memories WHERE project_id = $1)`, projectID)
		_, _ = db.ExecContext(ctx, `DELETE FROM lemn_memories WHERE project_id = $1`, projectID)
	}()

	vector := "[1," + strings.TrimSuffix(strings.Repeat("0,", 1023), ",") + "]"
	var dependencyID int
	if err := db.QueryRowContext(ctx, `
		INSERT INTO lemn_memories (state, project_id, confidence, category, summary, rationale, embedding, provenance)
		VALUES ('AUTHORITATIVE', $1, 1.0, 'architecture', 'The API is served by the internal service.', 'test fixture', $2::vector, '{}'::jsonb)
		RETURNING id`, projectID, vector).Scan(&dependencyID); err != nil {
		t.Fatalf("seed dependency memory: %v", err)
	}

	tests := []struct {
		name       string
		confidence float64
		evidence   []ToolCallEvidence
		wantReason string
	}{
		{name: "confidence auto-promotion", confidence: 0.99, wantReason: "confidence"},
		{name: "evidence auto-promotion", confidence: 0.4, evidence: []ToolCallEvidence{{Name: "edit", DiffText: "The API timeout is configured to 30 seconds."}}},
	}
	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			turn := TurnPayload{
				ID:                fmt.Sprintf("turn-%d", index),
				ProjectID:         projectID,
				UserMessage:       "Record the API timeout setting.",
				AssistantResponse: "The API timeout is configured to 30 seconds.",
				ToolCalls:         test.evidence,
			}
			extraction := ModelExtraction{
				MemoryWorthy:           true,
				Type:                   "architecture",
				Summary:                fmt.Sprintf("The API timeout is configured to 30 seconds for case %d.", index),
				Confidence:             test.confidence,
				DependsOn:              []int{dependencyID},
				DependencyCandidateIDs: []int{dependencyID},
				RelationCandidates:     []MatchTarget{},
				SummaryEmbedding:       embedding,
			}
			id, err := RouteExtraction(ctx, db, turn, extraction)
			if err != nil {
				t.Fatalf("RouteExtraction() error = %v", err)
			}
			var state string
			var provenanceJSON []byte
			if err := db.QueryRowContext(ctx, `SELECT state, provenance FROM lemn_memories WHERE id = $1`, id).Scan(&state, &provenanceJSON); err != nil {
				t.Fatalf("load promoted memory: %v", err)
			}
			if state != "AUTHORITATIVE" {
				t.Fatalf("state = %q, want AUTHORITATIVE", state)
			}
			var provenance map[string]interface{}
			if err := json.Unmarshal(provenanceJSON, &provenance); err != nil {
				t.Fatalf("decode provenance: %v", err)
			}
			if provenance["auto_promote_reason"] != test.wantReason && test.wantReason != "" {
				t.Fatalf("auto_promote_reason = %v, want %q", provenance["auto_promote_reason"], test.wantReason)
			}
			var edges int
			if err := db.QueryRowContext(ctx, `SELECT count(*) FROM lemn_edges WHERE source_id = $1 AND target_id = $2 AND relationship = 'depends_on'`, id, dependencyID).Scan(&edges); err != nil {
				t.Fatalf("query dependency edge: %v", err)
			}
			if edges != 1 {
				t.Fatalf("dependency edge count = %d, want 1", edges)
			}
		})
	}
}
