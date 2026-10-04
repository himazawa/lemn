package lemn

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestVisibleDuplicateRequiresReviewEvenWithEvidence(t *testing.T) {
	fixture := newRetractionFixture(t)
	t.Setenv("LEMN_ALLOW_CONFIDENCE_PROMOTION", "false")
	var schema string
	if err := fixture.db.QueryRowContext(fixture.ctx, `SELECT current_schema()`).Scan(&schema); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.db.ExecContext(fixture.ctx, `SELECT set_config('search_path', $1, false)`, schema+",public"); err != nil {
		t.Fatal(err)
	}
	fixture.exec(`ALTER TABLE lemn_memories ADD COLUMN summary TEXT, ADD COLUMN category TEXT,
		ADD COLUMN rationale TEXT, ADD COLUMN confidence REAL, ADD COLUMN created_at TIMESTAMPTZ DEFAULT now(),
		ADD COLUMN embedding vector(1024)`)
	vector := "[1," + strings.TrimSuffix(strings.Repeat("0,", 1023), ",") + "]"
	globalID := fixture.insert("AUTHORITATIVE", "global", nil)
	fixture.exec(`UPDATE lemn_memories SET summary = 'The user prefers concise answers.', confidence = 1,
		category = 'preference', embedding = $1::vector WHERE id = $2`, vector, globalID)
	embedding := make([]float32, 1024)
	embedding[0] = 1
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode(map[string]interface{}{"data": []map[string]interface{}{{"embedding": embedding}}})
	}))
	defer server.Close()
	t.Setenv("LEMN_EMBEDDING_URL", server.URL)
	id, err := RouteExtraction(fixture.ctx, fixture.db,
		TurnPayload{ID: "copy-global", ProjectID: "alpha", UserMessage: "I prefer concise answers.", ToolCalls: []ToolCallEvidence{{Name: "read", DiffText: "The user prefers concise answers."}}},
		ModelExtraction{MemoryWorthy: true, Type: "preference", Summary: "The user prefers concise answers.",
			Confidence: 1, SummaryEmbedding: embedding, Relation: "independent", RelationCandidates: []MatchTarget{}})
	if err != nil {
		t.Fatal(err)
	}
	provenance := fixture.memory(id, "PENDING_CONFIRMATION")
	ids := jsonIntSlice(provenance["duplicate_candidate_ids"])
	if len(ids) != 1 || ids[0] != globalID {
		t.Fatalf("duplicate candidates = %v, want global #%d", ids, globalID)
	}
	if _, err := SweepPending(fixture.ctx, fixture.db, true); err != nil {
		t.Fatal(err)
	}
	fixture.memory(id, "PENDING_CONFIRMATION")
	fixture.memory(globalID, "AUTHORITATIVE")
	retrieved, err := QueryAuthoritativeMemories(fixture.ctx, fixture.db, "concise answers", "alpha", 5)
	if err != nil {
		t.Fatal(err)
	}
	for _, memory := range retrieved {
		if memory.ID == id {
			t.Fatal("duplicate pending candidate appeared in authoritative retrieval")
		}
	}
	embedding[0], embedding[1] = 0, 1
	id, err = RouteExtraction(fixture.ctx, fixture.db,
		TurnPayload{ID: "unbacked", ProjectID: "alpha", UserMessage: "The API uses a 30 second timeout."},
		ModelExtraction{MemoryWorthy: true, Type: "architecture", Summary: "The API uses a 30 second timeout.",
			Confidence: 1, SummaryEmbedding: embedding, Relation: "independent", RelationCandidates: []MatchTarget{}})
	if err != nil {
		t.Fatal(err)
	}
	fixture.memory(id, "OBSERVED")
	if _, err := SweepPending(fixture.ctx, fixture.db, true); err != nil {
		t.Fatal(err)
	}
	fixture.memory(id, "OBSERVED")
	falseDuplicateID, err := RouteExtraction(fixture.ctx, fixture.db,
		TurnPayload{ID: "false-equivalence", ProjectID: "alpha", UserMessage: "The API uses a different timeout."},
		ModelExtraction{MemoryWorthy: true, Type: "architecture", Summary: "The API uses a different timeout.",
			Confidence: 1, ModelEquivalent: true, SummaryEmbedding: embedding, Relation: "independent", RelationCandidates: []MatchTarget{}})
	if err != nil {
		t.Fatal(err)
	}
	flagged := fixture.memory(falseDuplicateID, "PENDING_CONFIRMATION")
	if flagged["model_equivalent"] != true || flagged["duplicate_review_required"] != true {
		t.Fatalf("false equivalence lost its review metadata: %v", flagged)
	}
	if _, err := SweepPending(fixture.ctx, fixture.db, true); err != nil {
		t.Fatal(err)
	}
	fixture.memory(falseDuplicateID, "PENDING_CONFIRMATION")
	var encoded []byte
	if err := fixture.db.QueryRowContext(fixture.ctx, `SELECT provenance FROM lemn_memories WHERE id = $1`, globalID).Scan(&encoded); err != nil {
		t.Fatal(err)
	}
	var unchanged map[string]interface{}
	if err := json.Unmarshal(encoded, &unchanged); err != nil || unchanged["fixture"] != true {
		t.Fatalf("global provenance unexpectedly changed: %s, %v", encoded, err)
	}
}
