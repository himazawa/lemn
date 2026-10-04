package lemn

import (
	"context"
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"
)

func TestExtractionState(t *testing.T) {
	tests := []struct {
		name               string
		explicitCorrection bool
		relation           string
		evidence           bool
		want               string
	}{
		{name: "unsupported claim", want: "OBSERVED"},
		{name: "evidence-backed claim", evidence: true, want: "CANDIDATE"},
		{name: "correction requires review", explicitCorrection: true, evidence: true, want: "PENDING_CONFIRMATION"},
		{name: "proposed relation requires review", relation: "contradicts", want: "PENDING_CONFIRMATION"},
		{name: "independent relation uses evidence state", relation: "independent", evidence: true, want: "CANDIDATE"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := extractionState(test.explicitCorrection, test.relation, test.evidence); got != test.want {
				t.Fatalf("extractionState() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestAutoPromoteState(t *testing.T) {
	t.Setenv("LEMN_PROMOTE_THRESHOLD", "")
	t.Setenv("LEMN_ALLOW_CONFIDENCE_PROMOTION", "false")
	tests := []struct {
		name       string
		state      string
		confidence float64
		want       string
	}{
		{name: "evidence-backed independent claim is promoted", state: "CANDIDATE", confidence: 0.5, want: "AUTHORITATIVE"},
		{name: "low-confidence unbacked claim stays observed", state: "OBSERVED", confidence: 0.5, want: "OBSERVED"},
		{name: "high-confidence unbacked claim stays observed", state: "OBSERVED", confidence: 1.0, want: "OBSERVED"},
		{name: "confidence at threshold stays observed", state: "OBSERVED", confidence: 0.9, want: "OBSERVED"},
		{name: "high-confidence relation proposal stays gated", state: "PENDING_CONFIRMATION", confidence: 0.99, want: "PENDING_CONFIRMATION"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := autoPromoteState(test.state, test.confidence); got != test.want {
				t.Fatalf("autoPromoteState(%q, %v) = %q, want %q", test.state, test.confidence, got, test.want)
			}
		})
	}
}

func TestAutoPromoteStateCustomThreshold(t *testing.T) {
	t.Setenv("LEMN_ALLOW_CONFIDENCE_PROMOTION", "true")
	t.Setenv("LEMN_PROMOTE_THRESHOLD", "0.7")
	if got := autoPromoteState("OBSERVED", 0.75); got != "AUTHORITATIVE" {
		t.Fatalf("autoPromoteState(OBSERVED, 0.75) with threshold 0.7 = %q, want AUTHORITATIVE", got)
	}
	if got := autoPromoteState("OBSERVED", 0.6); got != "OBSERVED" {
		t.Fatalf("autoPromoteState(OBSERVED, 0.6) with threshold 0.7 = %q, want OBSERVED", got)
	}
	if got := autoPromoteState("PENDING_CONFIRMATION", 0.99); got != "PENDING_CONFIRMATION" {
		t.Fatalf("autoPromoteState(PENDING_CONFIRMATION, 0.99) = %q, want PENDING_CONFIRMATION", got)
	}
}

func TestAutoSupersedeThreshold(t *testing.T) {
	t.Setenv("LEMN_AUTO_SUPERSEDE_THRESHOLD", "")
	if got := autoSupersedeThreshold(); got != 0.95 {
		t.Fatalf("autoSupersedeThreshold() default = %v, want 0.95", got)
	}
	t.Setenv("LEMN_AUTO_SUPERSEDE_THRESHOLD", "0.8")
	if got := autoSupersedeThreshold(); got != 0.8 {
		t.Fatalf("autoSupersedeThreshold() with env = %v, want 0.8", got)
	}
	t.Setenv("LEMN_AUTO_SUPERSEDE_THRESHOLD", "not-a-number")
	if got := autoSupersedeThreshold(); got != 0.95 {
		t.Fatalf("autoSupersedeThreshold() with bad env = %v, want default 0.95", got)
	}
}

func TestShouldAutoConfirm(t *testing.T) {
	t.Setenv("LEMN_AUTO_SUPERSEDE_THRESHOLD", "")
	provWithTarget := map[string]interface{}{"proposed_target_id": float64(7)}
	provNoTarget := map[string]interface{}{}
	tests := []struct {
		name       string
		relation   string
		confidence float64
		evidence   bool
		provenance map[string]interface{}
		want       bool
	}{
		{name: "independent relation never auto-confirms", relation: "independent", confidence: 0.99, evidence: true, provenance: provWithTarget, want: false},
		{name: "supersedes without evidence stays gated", relation: "supersedes", confidence: 0.99, evidence: false, provenance: provWithTarget, want: false},
		{name: "supersedes below threshold stays gated", relation: "supersedes", confidence: 0.94, evidence: true, provenance: provWithTarget, want: false},
		{name: "supersedes at threshold with evidence auto-confirms", relation: "supersedes", confidence: 0.95, evidence: true, provenance: provWithTarget, want: true},
		{name: "contradicts with evidence auto-confirms", relation: "contradicts", confidence: 0.99, evidence: true, provenance: provWithTarget, want: true},
		{name: "relation without valid target stays gated", relation: "supersedes", confidence: 0.99, evidence: true, provenance: provNoTarget, want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := shouldAutoConfirm(test.relation, test.confidence, test.evidence, test.provenance); got != test.want {
				t.Fatalf("shouldAutoConfirm(%q, %v, %v) = %v, want %v", test.relation, test.confidence, test.evidence, got, test.want)
			}
		})
	}
}

func TestShouldAutoConfirmCustomThreshold(t *testing.T) {
	t.Setenv("LEMN_AUTO_SUPERSEDE_THRESHOLD", "0.8")
	prov := map[string]interface{}{"proposed_target_id": float64(2)}
	if !shouldAutoConfirm("supersedes", 0.8, true, prov) {
		t.Fatal("shouldAutoConfirm at custom threshold = false, want true")
	}
	if shouldAutoConfirm("supersedes", 0.79, true, prov) {
		t.Fatal("shouldAutoConfirm below custom threshold = true, want false")
	}
}

func TestStampAutoSupersedePreservesRequiredDependencies(t *testing.T) {
	mem := MemoryNode{Provenance: map[string]interface{}{
		"proposed_relation":  "supersedes",
		"proposed_target_id": float64(7),
		"depends_on":         []int{3, 7, 9},
	}}
	stampAutoSupersede(mem)
	if mem.Provenance["auto_supersede"] != true {
		t.Fatal("auto_supersede stamp missing")
	}
	deps := jsonIntSlice(mem.Provenance["depends_on"])
	if len(deps) != 3 || deps[0] != 3 || deps[1] != 7 || deps[2] != 9 {
		t.Fatalf("depends_on after stamp = %v, want [3 7 9]", deps)
	}
}

func TestResolveRelation(t *testing.T) {
	tests := []struct {
		name        string
		relation    string
		targetID    int
		targetValid bool
		want        string
		downgraded  bool
	}{
		{name: "independent stays independent", relation: "independent", want: "independent"},
		{name: "unknown relation becomes independent", relation: "refines", targetID: 3, targetValid: true, want: "independent"},
		{name: "valid target is kept", relation: "supersedes", targetID: 3, targetValid: true, want: "supersedes"},
		{name: "missing target downgrades", relation: "supersedes", want: "independent", downgraded: true},
		{name: "invalid target downgrades", relation: "contradicts", targetID: 3, want: "independent", downgraded: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			prov := map[string]interface{}{"proposed_relation": "stale", "proposed_target_id": 99}
			if got := resolveRelation(prov, test.relation, test.targetID, test.targetValid); got != test.want {
				t.Fatalf("resolveRelation() = %q, want %q", got, test.want)
			}
			if _, downgraded := prov["downgraded_relation"]; downgraded != test.downgraded {
				t.Fatalf("downgraded_relation present = %v, want %v", downgraded, test.downgraded)
			}
			if test.want == "independent" {
				if _, ok := prov["proposed_relation"]; ok {
					t.Fatal("proposed_relation left on an independent claim")
				}
				if _, ok := prov["proposed_target_id"]; ok {
					t.Fatal("proposed_target_id left on an independent claim")
				}
			} else if jsonInt(prov["proposed_target_id"]) != test.targetID {
				t.Fatalf("proposed_target_id = %v, want %d", prov["proposed_target_id"], test.targetID)
			}
		})
	}
}

func TestDependencyVisible(t *testing.T) {
	for _, tc := range []struct {
		depScope, scope string
		want            bool
	}{
		{"limn", "limn", true},
		{"global", "limn", true},
		{"limn", "global", false},
		{"other", "limn", false},
		{"global", "global", true},
	} {
		if got := dependencyVisible(tc.depScope, tc.scope); got != tc.want {
			t.Errorf("dependencyVisible(%q, %q) = %v, want %v", tc.depScope, tc.scope, got, tc.want)
		}
	}
}

func TestFilterStaleDependencies(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE lemn_memories (id INTEGER PRIMARY KEY, state TEXT, project_id TEXT)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO lemn_memories (id, state, project_id) VALUES (1, 'AUTHORITATIVE', 'global'), (2, 'NEEDS_REVALIDATION', 'project')`); err != nil {
		t.Fatal(err)
	}

	provenance := map[string]interface{}{"depends_on": []int{1, 2, 3, 0}}
	if err := filterStaleDependencies(context.Background(), db, "project", provenance); err == nil {
		t.Fatal("stale dependencies must require explicit review")
	}
	if got := jsonIntSlice(provenance["depends_on"]); len(got) != 4 || got[1] != 2 {
		t.Fatalf("depends_on = %v, want original [1 2 3 0]", got)
	}

	if _, err := db.Exec(`DROP TABLE lemn_memories`); err != nil {
		t.Fatal(err)
	}
	provenance = map[string]interface{}{"depends_on": []int{1}}
	if err := filterStaleDependencies(context.Background(), db, "project", provenance); err == nil {
		t.Fatal("filterStaleDependencies() error = nil, want database error")
	}
	if got := jsonIntSlice(provenance["depends_on"]); len(got) != 1 || got[0] != 1 {
		t.Fatalf("depends_on after database error = %v, want [1]", got)
	}
}

func TestIsMetaSummary(t *testing.T) {
	tests := map[string]bool{
		"The assistant explains the current memory retrieval limitations": true,
		"Assistant outlined the architecture":                             true,
		"Confirming memory #32 to supersedes memory #4":                   true,
		"The user prefers tabs over spaces":                               false,
		"The router uses a 25m idle timeout":                              false,
		"BUG_FIX #4 is stale as the threshold is now 0.9":                 false,
		"The assistant-facing API returns JSON":                           false,
	}
	for summary, want := range tests {
		if got := IsMetaSummary(summary); got != want {
			t.Errorf("IsMetaSummary(%q) = %v, want %v", summary, got, want)
		}
	}
}

func TestApprovedDependencyIDs(t *testing.T) {
	valid := map[int]struct{}{2: {}, 4: {}}
	got := approvedDependencyIDs([]int{2, 3, 2, 0, -1, 4}, valid)
	want := []int{2, 4}
	if len(got) != len(want) {
		t.Fatalf("approvedDependencyIDs() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("approvedDependencyIDs() = %v, want %v", got, want)
		}
	}
}
