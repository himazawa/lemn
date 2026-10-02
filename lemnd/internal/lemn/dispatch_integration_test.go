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

func TestRevalidationRequiresExplicitAttestationAndPreservesAudit(t *testing.T) {
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

	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("open test Postgres: %v", err)
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	projectID := fmt.Sprintf("revalidation-test-%d", time.Now().UnixNano())
	defer func() {
		_, _ = db.ExecContext(ctx, `DELETE FROM lemn_edges WHERE source_id IN (SELECT id FROM lemn_memories WHERE project_id = $1) OR target_id IN (SELECT id FROM lemn_memories WHERE project_id = $1)`, projectID)
		_, _ = db.ExecContext(ctx, `DELETE FROM lemn_memories WHERE project_id = $1`, projectID)
	}()

	unitX := "[1," + strings.TrimSuffix(strings.Repeat("0,", 1023), ",") + "]"
	unitY := "[0,1," + strings.TrimSuffix(strings.Repeat("0,", 1022), ",") + "]"
	insert := func(state, summary, vector string, provenance map[string]interface{}) int {
		t.Helper()
		provenanceJSON, err := json.Marshal(provenance)
		if err != nil {
			t.Fatalf("encode test provenance: %v", err)
		}
		var id int
		if err := db.QueryRowContext(ctx, `
			INSERT INTO lemn_memories (state, project_id, confidence, category, summary, rationale, embedding, provenance)
			VALUES ($1, $2, 0.99, 'architecture', $3, 'revalidation test fixture', $4::vector, $5::jsonb)
			RETURNING id`, state, projectID, summary, vector, provenanceJSON).Scan(&id); err != nil {
			t.Fatalf("insert %s test memory: %v", state, err)
		}
		return id
	}

	dependencyID := insert("AUTHORITATIVE", "The API currently uses HTTP/1.", unitY, map[string]interface{}{})
	dependentID := insert("AUTHORITATIVE", "The assistant describes the legacy HTTP/1 integration endpoint.", unitX, map[string]interface{}{
		"depends_on":         []int{dependencyID},
		"confirmed_relation": "independent",
	})
	if _, err := db.ExecContext(ctx, `INSERT INTO lemn_edges (source_id, target_id, relationship) VALUES ($1, $2, 'depends_on')`, dependentID, dependencyID); err != nil {
		t.Fatalf("insert dependency edge: %v", err)
	}
	replacementID := insert("PENDING_CONFIRMATION", "The API now uses HTTP/2.", unitX, map[string]interface{}{
		"proposed_relation":  "supersedes",
		"proposed_target_id": dependencyID,
	})
	if err := ConfirmPendingMemory(ctx, db, replacementID); err != nil {
		t.Fatalf("confirm replacement memory: %v", err)
	}

	var state string
	var provenanceJSON []byte
	if err := db.QueryRowContext(ctx, `SELECT state, provenance FROM lemn_memories WHERE id = $1`, dependentID).Scan(&state, &provenanceJSON); err != nil {
		t.Fatalf("load invalidated dependent: %v", err)
	}
	if state != "NEEDS_REVALIDATION" {
		t.Fatalf("dependent state = %q, want NEEDS_REVALIDATION", state)
	}
	var invalidation map[string]interface{}
	if err := json.Unmarshal(provenanceJSON, &invalidation); err != nil {
		t.Fatalf("decode invalidation provenance: %v", err)
	}
	if invalidation["revalidation_reason"] != "dependency_superseded" || jsonInt(invalidation["invalidated_by_memory_id"]) != replacementID || jsonInt(invalidation["invalidated_dependency_id"]) != dependencyID {
		t.Fatalf("invalidation provenance = %v, want superseded by #%d", invalidation, replacementID)
	}
	invalidatedAtText, ok := invalidation["invalidated_at"].(string)
	if !ok || invalidatedAtText == "" {
		t.Fatalf("invalidated_at = %v, want timestamp", invalidation["invalidated_at"])
	}
	invalidatedAt, err := time.Parse(time.RFC3339Nano, invalidatedAtText)
	if err != nil {
		t.Fatalf("parse invalidated_at %q: %v", invalidatedAtText, err)
	}

	if err := ConfirmPendingMemory(ctx, db, dependentID); err == nil {
		t.Fatal("ConfirmPendingMemory() promoted a memory needing revalidation")
	}
	if err := RevalidateMemory(ctx, db, dependentID, RevalidationDecision{}); err == nil {
		t.Fatal("RevalidateMemory() accepted an empty attestation")
	}
	actions, err := SweepPending(ctx, db, true)
	if err != nil {
		t.Fatalf("SweepPending() error = %v", err)
	}
	if len(actions) != 1 || actions[0].ID != dependentID || actions[0].Action != "keep" {
		t.Fatalf("SweepPending() actions = %+v, want one keep action for dependent", actions)
	}
	if err := RevalidateMemory(ctx, db, dependentID, RevalidationDecision{UserConfirmed: true}); err == nil {
		t.Fatal("RevalidateMemory() accepted a stale dependency without replacing it")
	}
	staleEvidence := RevalidationDecision{
		Evidence:           "The old HTTP/1 specification still mentions this endpoint.",
		EvidenceSource:     "https://docs.example.test/old-api",
		EvidenceObservedAt: invalidatedAt.Add(-time.Second),
	}
	if err := RevalidateMemory(ctx, db, dependentID, staleEvidence); err == nil {
		t.Fatal("RevalidateMemory() accepted evidence observed before invalidation")
	}

	before, err := QueryAuthoritativeMemories(ctx, db, "legacy HTTP/1 integration endpoint", projectID, 5)
	if err != nil {
		t.Fatalf("retrieve before revalidation: %v", err)
	}
	for _, memory := range before {
		if memory.ID == dependentID {
			t.Fatal("NEEDS_REVALIDATION memory appeared in authoritative retrieval")
		}
	}

	decision := RevalidationDecision{
		Evidence:            "Current API guidance confirms the HTTP/2 test endpoint.",
		EvidenceSource:      "https://docs.example.test/current-api#http2",
		EvidenceObservedAt:  time.Now().UTC(),
		Summary:             "Integration tests now invoke the HTTP/2 endpoint.",
		DependsOn:           []int{replacementID},
		ReplaceDependencies: true,
	}
	if err := RevalidateMemory(ctx, db, dependentID, decision); err != nil {
		t.Fatalf("RevalidateMemory() error = %v", err)
	}
	var summary string
	if err := db.QueryRowContext(ctx, `SELECT state, summary, provenance FROM lemn_memories WHERE id = $1`, dependentID).Scan(&state, &summary, &provenanceJSON); err != nil {
		t.Fatalf("load revalidated dependent: %v", err)
	}
	if state != "AUTHORITATIVE" || summary != decision.Summary {
		t.Fatalf("revalidated memory = state %q summary %q, want AUTHORITATIVE and %q", state, summary, decision.Summary)
	}
	var revalidated map[string]interface{}
	if err := json.Unmarshal(provenanceJSON, &revalidated); err != nil {
		t.Fatalf("decode revalidation provenance: %v", err)
	}
	if revalidated["confirmed_relation"] != "independent" {
		t.Fatalf("confirmed_relation changed during revalidation: %v", revalidated["confirmed_relation"])
	}
	history, ok := revalidated["revalidation_history"].([]interface{})
	if !ok || len(history) != 1 {
		t.Fatalf("revalidation_history = %v, want one audit entry", revalidated["revalidation_history"])
	}
	entry, ok := history[0].(map[string]interface{})
	if !ok || entry["reason"] != "dependency_superseded" || jsonInt(entry["invalidated_by_memory_id"]) != replacementID || jsonInt(entry["invalidated_dependency_id"]) != dependencyID || entry["evidence_note"] != decision.Evidence || entry["evidence_source"] != decision.EvidenceSource || entry["evidence_observed_at"] != decision.EvidenceObservedAt.Format(time.RFC3339Nano) || entry["invalidated_at"] != invalidatedAtText || entry["user_confirmed"] != false {
		t.Fatalf("revalidation history entry = %v, missing attestation or invalidation details", history[0])
	}
	var remainingDependencies int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM lemn_edges WHERE source_id = $1 AND relationship = 'depends_on'`, dependentID).Scan(&remainingDependencies); err != nil {
		t.Fatalf("query remaining dependencies: %v", err)
	}
	if remainingDependencies != 1 {
		t.Fatalf("remaining dependencies = %d, want one replacement dependency", remainingDependencies)
	}
	var dependencyTargetID int
	if err := db.QueryRowContext(ctx, `SELECT target_id FROM lemn_edges WHERE source_id = $1 AND relationship = 'depends_on'`, dependentID).Scan(&dependencyTargetID); err != nil {
		t.Fatalf("load replacement dependency: %v", err)
	}
	if dependencyTargetID != replacementID {
		t.Fatalf("replacement dependency target = %d, want %d", dependencyTargetID, replacementID)
	}
	after, err := QueryAuthoritativeMemories(ctx, db, "HTTP/2 integration endpoint", projectID, 5)
	if err != nil {
		t.Fatalf("retrieve after revalidation: %v", err)
	}
	found := false
	for _, memory := range after {
		found = found || memory.ID == dependentID
	}
	if !found {
		t.Fatal("revalidated memory was not restored to authoritative retrieval")
	}
}
