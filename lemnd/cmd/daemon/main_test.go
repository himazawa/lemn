package main

import (
	"testing"

	"lemnd/internal/lemn"
)

func TestValidateRelationChoice(t *testing.T) {
	candidates := []lemn.MatchTarget{{ID: 12}, {ID: 24}}
	tests := []struct {
		name       string
		relation   string
		targetID   int
		wantRel    string
		wantTarget int
	}{
		{name: "valid supersede candidate", relation: "supersedes", targetID: 12, wantRel: "supersedes", wantTarget: 12},
		{name: "valid contradiction candidate", relation: "contradicts", targetID: 24, wantRel: "contradicts", wantTarget: 24},
		{name: "unknown target becomes independent", relation: "supersedes", targetID: 99, wantRel: "independent"},
		{name: "independent discards target", relation: "independent", targetID: 12, wantRel: "independent"},
		{name: "unknown relation becomes independent", relation: "related", targetID: 12, wantRel: "independent"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			relation, targetID, err := validateRelationChoice(test.relation, test.targetID, candidates)
			if err != nil {
				t.Fatalf("validateRelationChoice() error = %v", err)
			}
			if relation != test.wantRel || targetID != test.wantTarget {
				t.Fatalf("validateRelationChoice() = (%q, %d), want (%q, %d)", relation, targetID, test.wantRel, test.wantTarget)
			}
		})
	}
}
