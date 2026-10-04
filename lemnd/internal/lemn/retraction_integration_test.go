package lemn

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/lib/pq"
)

type retractionFixture struct {
	t   *testing.T
	db  *sql.DB
	ctx context.Context
}

func newRetractionFixture(t *testing.T) *retractionFixture {
	t.Helper()
	dsn := os.Getenv("LEMN_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set LEMN_TEST_POSTGRES_DSN to run PostgreSQL retraction tests")
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	schema := pq.QuoteIdentifier(fmt.Sprintf("retraction_test_%d", time.Now().UnixNano()))
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cleanupCancel()
		if _, err := db.ExecContext(cleanupCtx, `DROP SCHEMA IF EXISTS `+schema+` CASCADE`); err != nil {
			t.Errorf("clean up test schema: %v", err)
		}
		db.Close()
		cancel()
	})
	for _, statement := range []string{
		`CREATE SCHEMA ` + schema,
		`SET search_path TO ` + schema,
		`CREATE TABLE lemn_memories (
			id SERIAL PRIMARY KEY, state TEXT NOT NULL, project_id TEXT NOT NULL,
			provenance JSONB NOT NULL
		)`,
		`CREATE TABLE lemn_edges (
			source_id INT REFERENCES lemn_memories(id), target_id INT REFERENCES lemn_memories(id),
			relationship TEXT NOT NULL, PRIMARY KEY (source_id, target_id)
		)`,
	} {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatalf("initialize test schema: %v", err)
		}
	}
	return &retractionFixture{t: t, db: db, ctx: ctx}
}

func (fixture *retractionFixture) insert(state, scope string, provenance map[string]interface{}) int {
	fixture.t.Helper()
	if provenance == nil {
		provenance = map[string]interface{}{"fixture": true}
	}
	encoded, err := json.Marshal(provenance)
	if err != nil {
		fixture.t.Fatal(err)
	}
	var id int
	if err := fixture.db.QueryRowContext(fixture.ctx, `
		INSERT INTO lemn_memories (state, project_id, provenance) VALUES ($1, $2, $3) RETURNING id`,
		state, scope, string(encoded)).Scan(&id); err != nil {
		fixture.t.Fatal(err)
	}
	return id
}

func (fixture *retractionFixture) exec(statement string, args ...interface{}) {
	fixture.t.Helper()
	if _, err := fixture.db.ExecContext(fixture.ctx, statement, args...); err != nil {
		fixture.t.Fatal(err)
	}
}

func (fixture *retractionFixture) edge(sourceID, targetID int, relation string) {
	fixture.t.Helper()
	fixture.exec(`INSERT INTO lemn_edges VALUES ($1, $2, $3)`, sourceID, targetID, relation)
}

func (fixture *retractionFixture) memory(id int, wantState string) map[string]interface{} {
	fixture.t.Helper()
	var state string
	var encoded []byte
	if err := fixture.db.QueryRowContext(fixture.ctx, `SELECT state, provenance FROM lemn_memories WHERE id = $1`, id).Scan(&state, &encoded); err != nil {
		fixture.t.Fatal(err)
	}
	if state != wantState {
		fixture.t.Fatalf("memory #%d state = %s, want %s", id, state, wantState)
	}
	var provenance map[string]interface{}
	if err := json.Unmarshal(encoded, &provenance); err != nil {
		fixture.t.Fatal(err)
	}
	return provenance
}

func (fixture *retractionFixture) snapshot() string {
	fixture.t.Helper()
	var snapshot string
	if err := fixture.db.QueryRowContext(fixture.ctx, `
		SELECT jsonb_build_object(
			'memories', (SELECT jsonb_agg(to_jsonb(m) ORDER BY id) FROM lemn_memories m),
			'edges', (SELECT jsonb_agg(to_jsonb(e) ORDER BY source_id, target_id) FROM lemn_edges e)
		)::text`).Scan(&snapshot); err != nil {
		fixture.t.Fatal(err)
	}
	return snapshot
}

func TestRetractionAuthoritativeCascade(t *testing.T) {
	fixture := newRetractionFixture(t)
	root := fixture.insert("AUTHORITATIVE", GlobalScope, map[string]interface{}{"confirmed_relation": "supersedes", "fixture": true})
	globalDependent := fixture.insert("AUTHORITATIVE", GlobalScope, nil)
	projectDependent := fixture.insert("AUTHORITATIVE", "project", nil)
	otherProjectDependent := fixture.insert("AUTHORITATIVE", "other-project", nil)
	pending := fixture.insert("PENDING_CONFIRMATION", "project", nil)
	quarantined := fixture.insert("NEEDS_REVALIDATION", "project", nil)
	unrelated := fixture.insert("AUTHORITATIVE", "project", nil)
	historical := fixture.insert("SUPERSEDED", GlobalScope, nil)
	fixture.edge(root, historical, "supersedes")
	fixture.edge(globalDependent, root, "depends_on")
	fixture.edge(projectDependent, globalDependent, "depends_on")
	fixture.edge(otherProjectDependent, root, "depends_on")
	fixture.edge(pending, root, "depends_on")
	fixture.edge(quarantined, root, "depends_on")

	if err := RejectMemory(fixture.ctx, fixture.db, root, "  administrator correction  "); err != nil {
		t.Fatal(err)
	}
	provenance := fixture.memory(root, "REJECTED")
	if provenance["rejection_reason"] != "administrator correction" || provenance["rejected_from_state"] != "AUTHORITATIVE" || provenance["review_actor"] != "human" || provenance["fixture"] != true || provenance["confirmed_relation"] != "supersedes" {
		t.Fatalf("retraction audit = %v", provenance)
	}
	if _, err := time.Parse(time.RFC3339Nano, fmt.Sprint(provenance["rejected_at"])); err != nil {
		t.Fatalf("rejected_at = %v: %v", provenance["rejected_at"], err)
	}
	if provenance["reviewed_at"] != provenance["rejected_at"] {
		t.Fatalf("review/rejection timestamps differ: %v", provenance)
	}
	for _, id := range []int{globalDependent, projectDependent, otherProjectDependent} {
		dependent := fixture.memory(id, "NEEDS_REVALIDATION")
		if dependent["revalidation_reason"] != "dependency_rejected" || jsonInt(dependent["invalidated_by_memory_id"]) != root || jsonInt(dependent["invalidated_dependency_id"]) != root || dependent["invalidated_at"] != provenance["rejected_at"] || dependent["fixture"] != true {
			t.Fatalf("dependent #%d audit = %v", id, dependent)
		}
	}
	fixture.memory(pending, "PENDING_CONFIRMATION")
	fixture.memory(quarantined, "NEEDS_REVALIDATION")
	fixture.memory(unrelated, "AUTHORITATIVE")
	fixture.memory(historical, "SUPERSEDED")
	var edges int
	if err := fixture.db.QueryRowContext(fixture.ctx, `SELECT count(*) FROM lemn_edges`).Scan(&edges); err != nil || edges != 6 {
		t.Fatalf("preserved edge count = %d, error = %v", edges, err)
	}
	var relation string
	if err := fixture.db.QueryRowContext(fixture.ctx, `SELECT relationship FROM lemn_edges WHERE source_id = $1 AND target_id = $2`, root, historical).Scan(&relation); err != nil || relation != "supersedes" {
		t.Fatalf("historical relation = %q, error = %v", relation, err)
	}
}

func TestRetractionValidationAndPendingDefault(t *testing.T) {
	fixture := newRetractionFixture(t)
	root := fixture.insert("AUTHORITATIVE", GlobalScope, nil)
	for _, reason := range []string{"", " \t\n"} {
		before := fixture.snapshot()
		if err := RejectMemory(fixture.ctx, fixture.db, root, reason); err == nil {
			t.Fatal("authoritative retraction accepted blank reason")
		}
		if after := fixture.snapshot(); before != after {
			t.Fatalf("blank reason changed database: %s", after)
		}
	}
	for _, state := range []string{"OBSERVED", "CANDIDATE", "PENDING", "PENDING_CONFIRMATION", "NEEDS_REVALIDATION"} {
		id := fixture.insert(state, "project", nil)
		if err := RejectMemory(fixture.ctx, fixture.db, id, ""); err != nil {
			t.Fatal(err)
		}
		provenance := fixture.memory(id, "REJECTED")
		if provenance["rejection_reason"] == "" || provenance["rejected_from_state"] != state {
			t.Fatalf("pending rejection audit = %v", provenance)
		}
	}
	for _, state := range []string{"REJECTED", "SUPERSEDED", "CONTRADICTED"} {
		id := fixture.insert(state, "project", nil)
		before := fixture.snapshot()
		if err := RejectMemory(fixture.ctx, fixture.db, id, "admin"); err == nil || before != fixture.snapshot() {
			t.Fatalf("rejection of %s did not fail unchanged: %v", state, err)
		}
	}
	before := fixture.snapshot()
	if err := RejectMemory(fixture.ctx, fixture.db, root+10000, "admin"); err == nil || before != fixture.snapshot() {
		t.Fatalf("missing ID did not fail unchanged: %v", err)
	}
}

func TestConfirmationInvalidationClosureRollback(t *testing.T) {
	for _, relation := range []string{"supersedes", "contradicts"} {
		t.Run(relation, func(t *testing.T) {
			fixture := newRetractionFixture(t)
			root := fixture.insert("AUTHORITATIVE", "project", nil)
			first := fixture.insert("AUTHORITATIVE", "project", nil)
			second := fixture.insert("AUTHORITATIVE", "project", nil)
			old := fixture.insert("SUPERSEDED", "project", nil)
			pending := fixture.insert("PENDING_CONFIRMATION", "project", map[string]interface{}{
				"proposed_relation": relation, "proposed_target_id": root, "depends_on": []int{second},
			})
			fixture.edge(first, root, "depends_on")
			fixture.edge(second, first, "depends_on")
			fixture.edge(pending, old, "supersedes")
			fixture.edge(pending, first, "depends_on")
			before := fixture.snapshot()
			err := ConfirmPendingMemory(fixture.ctx, fixture.db, pending, ConfirmationDecision{AutoConfirmed: true})
			if err == nil || !strings.Contains(err.Error(), "would be invalidated") {
				t.Fatalf("confirmation error = %v, want invalidation closure refusal", err)
			}
			if after := fixture.snapshot(); after != before {
				t.Fatalf("confirmation failed to roll back state, provenance, or edges: %s", after)
			}
		})
	}
}

func TestConfirmationReviewProvenance(t *testing.T) {
	fixture := newRetractionFixture(t)
	for _, auto := range []bool{false, true} {
		id := fixture.insert("CANDIDATE", "project", nil)
		var err error
		actor := "human"
		if auto {
			actor = "auto"
			err = ConfirmPendingMemory(fixture.ctx, fixture.db, id, ConfirmationDecision{AutoConfirmed: true})
		} else {
			err = ConfirmPendingMemory(fixture.ctx, fixture.db, id)
		}
		if err != nil {
			t.Fatal(err)
		}
		provenance := fixture.memory(id, "AUTHORITATIVE")
		if provenance["review_actor"] != actor || provenance["auto_confirmed"] != auto || provenance["fixture"] != true {
			t.Fatalf("review provenance = %v", provenance)
		}
		if _, err := time.Parse(time.RFC3339Nano, fmt.Sprint(provenance["reviewed_at"])); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRetractionCascadeFailureRollback(t *testing.T) {
	fixture := newRetractionFixture(t)
	root := fixture.insert("AUTHORITATIVE", GlobalScope, nil)
	dependent := fixture.insert("AUTHORITATIVE", "project", nil)
	fixture.edge(dependent, root, "depends_on")
	fixture.exec(fmt.Sprintf(`ALTER TABLE lemn_memories ADD CONSTRAINT fail_cascade CHECK (id <> %d OR state <> 'NEEDS_REVALIDATION')`, dependent))
	before := fixture.snapshot()
	if err := RejectMemory(fixture.ctx, fixture.db, root, "admin"); err == nil {
		t.Fatal("retraction ignored cascade failure")
	}
	if after := fixture.snapshot(); before != after {
		t.Fatalf("failed cascade retained state or audit changes: %s", after)
	}
}

func TestConfirmationPostCascadeFailureRollback(t *testing.T) {
	fixture := newRetractionFixture(t)
	root := fixture.insert("AUTHORITATIVE", "project", nil)
	dependent := fixture.insert("AUTHORITATIVE", "project", nil)
	pending := fixture.insert("CANDIDATE", "project", nil)
	fixture.edge(dependent, root, "depends_on")
	fixture.exec(fmt.Sprintf(`ALTER TABLE lemn_memories ADD CONSTRAINT fail_promotion CHECK (id <> %d OR state <> 'AUTHORITATIVE')`, pending))
	before := fixture.snapshot()
	err := ConfirmPendingMemory(fixture.ctx, fixture.db, pending, ConfirmationDecision{Relation: "supersedes", TargetID: root, AutoConfirmed: true})
	if err == nil || !strings.Contains(err.Error(), "failed to promote") {
		t.Fatalf("confirmation error = %v, want promotion failure after cascade", err)
	}
	if after := fixture.snapshot(); before != after {
		t.Fatalf("failed promotion retained cascade, audit, or relation changes: %s", after)
	}
}

func TestConfirmationRechecksDependenciesAfterCascade(t *testing.T) {
	fixture := newRetractionFixture(t)
	root := fixture.insert("AUTHORITATIVE", "project", nil)
	dependency := fixture.insert("AUTHORITATIVE", GlobalScope, nil)
	pending := fixture.insert("CANDIDATE", "project", nil)
	fixture.exec(fmt.Sprintf(`
		CREATE FUNCTION invalidate_review_dependency() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			IF NEW.id = %d AND NEW.state = 'SUPERSEDED' THEN
				UPDATE lemn_memories SET state = 'NEEDS_REVALIDATION' WHERE id = %d;
			END IF;
			RETURN NEW;
		END $$`, root, dependency))
	fixture.exec(`CREATE TRIGGER invalidate_review_dependency AFTER UPDATE ON lemn_memories
		FOR EACH ROW EXECUTE FUNCTION invalidate_review_dependency()`)
	before := fixture.snapshot()
	err := ConfirmPendingMemory(fixture.ctx, fixture.db, pending, ConfirmationDecision{
		Relation: "supersedes", TargetID: root, DependsOn: []int{dependency}, ReplaceDependencies: true,
	})
	if err == nil || !strings.Contains(err.Error(), "no longer authoritative after confirmation cascade") {
		t.Fatalf("confirmation error = %v, want post-cascade dependency refusal", err)
	}
	if after := fixture.snapshot(); before != after {
		t.Fatalf("post-cascade refusal did not roll back: %s", after)
	}
}

func TestConfirmationSafeDependencyPreservesRelations(t *testing.T) {
	fixture := newRetractionFixture(t)
	root := fixture.insert("AUTHORITATIVE", "project", nil)
	dependency := fixture.insert("AUTHORITATIVE", GlobalScope, nil)
	old := fixture.insert("CONTRADICTED", "project", nil)
	pending := fixture.insert("CANDIDATE", "project", nil)
	fixture.edge(pending, old, "contradicts")
	if err := ConfirmPendingMemory(fixture.ctx, fixture.db, pending, ConfirmationDecision{
		Relation: "supersedes", TargetID: root, DependsOn: []int{dependency}, ReplaceDependencies: true,
	}); err != nil {
		t.Fatal(err)
	}
	fixture.memory(root, "SUPERSEDED")
	fixture.memory(dependency, "AUTHORITATIVE")
	fixture.memory(pending, "AUTHORITATIVE")
	for target, wantRelation := range map[int]string{root: "supersedes", dependency: "depends_on", old: "contradicts"} {
		var relation string
		if err := fixture.db.QueryRowContext(fixture.ctx, `SELECT relationship FROM lemn_edges WHERE source_id = $1 AND target_id = $2`, pending, target).Scan(&relation); err != nil || relation != wantRelation {
			t.Fatalf("relation to #%d = %q, want %q, error = %v", target, relation, wantRelation, err)
		}
	}
}

func TestRejectMemoryRequiresDatabase(t *testing.T) {
	if err := RejectMemory(context.Background(), nil, 1, "admin"); err == nil {
		t.Fatal("RejectMemory accepted a nil database")
	}
}
