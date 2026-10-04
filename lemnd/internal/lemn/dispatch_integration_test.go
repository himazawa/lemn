package lemn

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
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
	t.Setenv("LEMN_ALLOW_CONFIDENCE_PROMOTION", "true")

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

	vector := "[0,1," + strings.TrimSuffix(strings.Repeat("0,", 1022), ",") + "]"
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
			if index > 0 {
				if _, err := db.ExecContext(ctx, `UPDATE lemn_memories SET state = 'REJECTED' WHERE project_id = $1 AND id <> $2`, projectID, dependencyID); err != nil {
					t.Fatal(err)
				}
			}
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

func TestSweepPendingAppliesPromoteAndAutoConfirm(t *testing.T) {
	dsn := os.Getenv("LEMN_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set LEMN_TEST_POSTGRES_DSN to an isolated database initialized from schema.sql")
	}
	t.Setenv("LEMN_PROMOTE_THRESHOLD", "0.9")
	t.Setenv("LEMN_ALLOW_CONFIDENCE_PROMOTION", "true")
	t.Setenv("LEMN_AUTO_SUPERSEDE_THRESHOLD", "0.95")

	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("open test Postgres: %v", err)
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	projectID := fmt.Sprintf("sweep-promotion-test-%d", time.Now().UnixNano())
	defer func() {
		_, _ = db.ExecContext(ctx, `DELETE FROM lemn_edges WHERE source_id IN (SELECT id FROM lemn_memories WHERE project_id = $1) OR target_id IN (SELECT id FROM lemn_memories WHERE project_id = $1)`, projectID)
		_, _ = db.ExecContext(ctx, `DELETE FROM lemn_memories WHERE project_id = $1`, projectID)
	}()
	vector := "[1," + strings.TrimSuffix(strings.Repeat("0,", 1023), ",") + "]"
	insert := func(state string, confidence float64, summary string, provenance map[string]interface{}) int {
		t.Helper()
		provenanceJSON, err := json.Marshal(provenance)
		if err != nil {
			t.Fatalf("encode provenance: %v", err)
		}
		var id int
		err = db.QueryRowContext(ctx, `
			INSERT INTO lemn_memories (state, project_id, confidence, category, summary, rationale, embedding, provenance)
			VALUES ($1, $2, $3, 'architecture', $4, 'sweep test fixture', $5::vector, $6::jsonb)
			RETURNING id`, state, projectID, confidence, summary, vector, provenanceJSON).Scan(&id)
		if err != nil {
			t.Fatalf("insert %s fixture: %v", state, err)
		}
		return id
	}

	targetID := insert("AUTHORITATIVE", 1, "The API uses HTTP/1.", map[string]interface{}{})
	promoteID := insert("CANDIDATE", 0.99, "The API timeout is 30 seconds.", map[string]interface{}{})
	orthogonal := "[0,1," + strings.TrimSuffix(strings.Repeat("0,", 1022), ",") + "]"
	if _, err := db.ExecContext(ctx, `UPDATE lemn_memories SET embedding = $1::vector WHERE id = $2`, orthogonal, promoteID); err != nil {
		t.Fatal(err)
	}
	autoConfirmID := insert("PENDING_CONFIRMATION", 0.99, "The API now uses HTTP/2.", map[string]interface{}{
		"proposed_relation":  "supersedes",
		"proposed_target_id": targetID,
		"evidence_source":    "test evidence",
	})

	actions, err := SweepPending(ctx, db, true)
	if err != nil {
		t.Fatalf("SweepPending() error = %v", err)
	}
	wantActions := map[int]string{promoteID: "promote", autoConfirmID: "auto-confirm"}
	for _, action := range actions {
		want, ok := wantActions[action.ID]
		if !ok {
			continue
		}
		if action.Action != want || action.Err != nil {
			t.Errorf("SweepPending() action for #%d = (%q, %v), want (%q, nil)", action.ID, action.Action, action.Err, want)
		}
		delete(wantActions, action.ID)
	}
	if len(wantActions) != 0 {
		t.Fatalf("SweepPending() did not return actions for %v", wantActions)
	}

	for id, wantState := range map[int]string{promoteID: "AUTHORITATIVE", autoConfirmID: "AUTHORITATIVE", targetID: "SUPERSEDED"} {
		var state string
		if err := db.QueryRowContext(ctx, `SELECT state FROM lemn_memories WHERE id = $1`, id).Scan(&state); err != nil {
			t.Fatalf("load state for #%d: %v", id, err)
		}
		if state != wantState {
			t.Errorf("state for #%d = %q, want %q", id, state, wantState)
		}
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
	verifierAt := func(observedAt time.Time) evidenceVerifier {
		return func(_ context.Context, sourceURL, quote string) (verifiedEvidence, error) {
			sourceBody := "verified test source: " + quote
			sourceDigest := sha256.Sum256([]byte(sourceBody))
			quoteDigest := sha256.Sum256([]byte(strings.TrimSpace(quote)))
			return verifiedEvidence{
				SourceURL:    sourceURL,
				Quote:        strings.TrimSpace(quote),
				ObservedAt:   observedAt,
				SourceSHA256: hex.EncodeToString(sourceDigest[:]),
				QuoteSHA256:  hex.EncodeToString(quoteDigest[:]),
				ContentType:  "text/plain",
			}, nil
		}
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
	t.Setenv("LEMN_REVALIDATION_ALLOWED_HOSTS", "")
	unapprovedSource := RevalidationDecision{
		Evidence:       "Current API guidance confirms the HTTP/2 test endpoint.",
		EvidenceSource: "https://docs.example.test/current-api#http2",
	}
	if err := RevalidateMemory(ctx, db, dependentID, unapprovedSource); err == nil || !strings.Contains(err.Error(), "no evidence hosts are approved") {
		t.Fatalf("RevalidateMemory() with no approved hosts error = %v, want fail-closed allowlist error", err)
	}
	if err := db.QueryRowContext(ctx, `SELECT state FROM lemn_memories WHERE id = $1`, dependentID).Scan(&state); err != nil {
		t.Fatalf("check state after unapproved-source rejection: %v", err)
	}
	if state != "NEEDS_REVALIDATION" {
		t.Fatalf("state after unapproved-source rejection = %q, want NEEDS_REVALIDATION", state)
	}
	staleEvidence := RevalidationDecision{
		Evidence:       "The old HTTP/1 specification still mentions this endpoint.",
		EvidenceSource: "https://docs.example.test/old-api",
	}
	if err := revalidateMemoryWithVerifier(ctx, db, dependentID, staleEvidence, verifierAt(invalidatedAt.Add(-time.Second))); err == nil {
		t.Fatal("RevalidateMemory() accepted evidence observed before invalidation")
	}
	maxAge := defaultRevalidationEvidenceMaxAge
	if configuredAge, err := revalidationEvidenceMaxAge(); err != nil {
		t.Fatalf("read maximum evidence age: %v", err)
	} else {
		maxAge = configuredAge
	}
	now := time.Now().UTC()
	agedInvalidation := now.Add(-maxAge - 2*time.Second)
	agedEvidenceAt := now.Add(-maxAge - time.Second)
	var agedProvenance []byte
	if err := db.QueryRowContext(ctx, `
		UPDATE lemn_memories
		SET provenance = jsonb_set(provenance, '{invalidated_at}', to_jsonb($1::text))
		WHERE id = $2
		RETURNING provenance`, agedInvalidation.Format(time.RFC3339Nano), dependentID).Scan(&agedProvenance); err != nil {
		t.Fatalf("age invalidation timestamp for stale-evidence case: %v", err)
	}
	agedEvidence := RevalidationDecision{
		Evidence:       "The source was checked after invalidation, but too long ago.",
		EvidenceSource: "https://docs.example.test/old-revalidation-window",
	}
	if err := revalidateMemoryWithVerifier(ctx, db, dependentID, agedEvidence, verifierAt(agedEvidenceAt)); err == nil {
		t.Fatal("RevalidateMemory() accepted evidence older than the configured maximum age")
	}
	if err := db.QueryRowContext(ctx, `SELECT state FROM lemn_memories WHERE id = $1`, dependentID).Scan(&state); err != nil {
		t.Fatalf("check state after expired evidence rejection: %v", err)
	}
	if state != "NEEDS_REVALIDATION" {
		t.Fatalf("state after expired evidence rejection = %q, want NEEDS_REVALIDATION", state)
	}
	if _, err := db.ExecContext(ctx, `UPDATE lemn_memories SET provenance = $1 WHERE id = $2`, provenanceJSON, dependentID); err != nil {
		t.Fatalf("restore original invalidation provenance: %v", err)
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
		Summary:             "Integration tests now invoke the HTTP/2 endpoint.",
		DependsOn:           []int{replacementID},
		ReplaceDependencies: true,
	}
	acceptedEvidenceAt := time.Now().UTC()
	if err := revalidateMemoryWithVerifier(ctx, db, dependentID, decision, verifierAt(acceptedEvidenceAt)); err != nil {
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
	sourceDigest := sha256.Sum256([]byte("verified test source: " + decision.Evidence))
	quoteDigest := sha256.Sum256([]byte(strings.TrimSpace(decision.Evidence)))
	if !ok || entry["reason"] != "dependency_superseded" || jsonInt(entry["invalidated_by_memory_id"]) != replacementID || jsonInt(entry["invalidated_dependency_id"]) != dependencyID || entry["evidence_note"] != decision.Evidence || entry["evidence_note_sha256"] != hex.EncodeToString(quoteDigest[:]) || entry["evidence_source"] != decision.EvidenceSource || entry["evidence_source_sha256"] != hex.EncodeToString(sourceDigest[:]) || entry["evidence_source_content_type"] != "text/plain" || entry["evidence_observed_at"] != acceptedEvidenceAt.Format(time.RFC3339Nano) || entry["invalidated_at"] != invalidatedAtText || entry["user_confirmed"] != false {
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

	manualID := insert("NEEDS_REVALIDATION", "The migration decision was reconfirmed directly with the user.", unitY, map[string]interface{}{
		"revalidation_reason": "dependency_contradicted",
		"invalidated_at":      time.Now().UTC().Add(-time.Hour).Format(time.RFC3339Nano),
	})
	if err := RevalidateMemory(ctx, db, manualID, RevalidationDecision{UserConfirmed: true}); err != nil {
		t.Fatalf("RevalidateMemory() explicit user confirmation error = %v", err)
	}
	var manualState string
	var manualProvenanceJSON []byte
	if err := db.QueryRowContext(ctx, `SELECT state, provenance FROM lemn_memories WHERE id = $1`, manualID).Scan(&manualState, &manualProvenanceJSON); err != nil {
		t.Fatalf("load explicitly confirmed memory: %v", err)
	}
	var manualProvenance map[string]interface{}
	if err := json.Unmarshal(manualProvenanceJSON, &manualProvenance); err != nil {
		t.Fatalf("decode explicit-confirmation provenance: %v", err)
	}
	manualHistory, ok := manualProvenance["revalidation_history"].([]interface{})
	if manualState != "AUTHORITATIVE" || !ok || len(manualHistory) != 1 {
		t.Fatalf("explicit confirmation state/history = %q/%v, want AUTHORITATIVE/one entry", manualState, manualProvenance["revalidation_history"])
	}
	manualEntry, ok := manualHistory[0].(map[string]interface{})
	if !ok || manualEntry["user_confirmed"] != true {
		t.Fatalf("explicit confirmation audit = %v, want user_confirmed=true", manualHistory[0])
	}
	if _, exists := manualEntry["evidence_source_sha256"]; exists {
		t.Fatalf("explicit confirmation audit unexpectedly claims source verification: %v", manualEntry)
	}
}
