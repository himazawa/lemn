package lemn

import (
	"context"
	"testing"
)

func TestJsonInt(t *testing.T) {
	tests := []struct {
		name  string
		value interface{}
		want  int
	}{
		{name: "nil", value: nil, want: 0},
		{name: "int", value: 42, want: 42},
		{name: "json number decodes as float64", value: float64(7), want: 7},
		{name: "fractional value is not an id", value: 1.5, want: 0},
		{name: "numeric string is not an id", value: "7", want: 0},
		{name: "bool is not an id", value: true, want: 0},
		{name: "nested value is not an id", value: map[string]interface{}{"id": 3}, want: 0},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := jsonInt(test.value); got != test.want {
				t.Fatalf("jsonInt(%v) = %d, want %d", test.value, got, test.want)
			}
		})
	}
}

func TestJsonIntSlice(t *testing.T) {
	tests := []struct {
		name  string
		value interface{}
		want  []int
	}{
		{name: "nil", value: nil, want: nil},
		{
			name: "json array of numbers", value: []interface{}{float64(2), float64(4)},
			want: []int{2, 4},
		},
		{
			name: "go slice of ints", value: []int{1, 3},
			want: []int{1, 3},
		},
		{name: "scalar is not a slice", value: 5, want: nil},
		{name: "empty array", value: []interface{}{}, want: []int{}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := jsonIntSlice(test.value)
			if len(got) != len(test.want) {
				t.Fatalf("jsonIntSlice(%v) = %v, want %v", test.value, got, test.want)
			}
			for i := range test.want {
				if got[i] != test.want[i] {
					t.Fatalf("jsonIntSlice(%v) = %v, want %v", test.value, got, test.want)
				}
			}
		})
	}
}

// The no-tool-call path must not touch the embedder, so it is testable
// offline. Paths with tool calls require a live embedding service and are
// covered by the integration tests.
func TestMeasureEvidenceNoToolCalls(t *testing.T) {
	mem := MemoryNode{Summary: "the build uses bazel"}
	sig, err := measureEvidence(context.Background(), mem, nil)
	if err != nil {
		t.Fatalf("measureEvidence() error = %v", err)
	}
	if sig.Relevant || sig.Tool != "" || sig.Similarity != 0 {
		t.Fatalf("measureEvidence() = %+v, want zero signal with no tool calls", sig)
	}
}

func TestEvidenceSignalFromScores(t *testing.T) {
	tests := []struct {
		name         string
		tools        []string
		scores       []float64
		wantTool     string
		wantScore    float64
		wantRelevant bool
	}{
		{name: "exact threshold is relevant", tools: []string{"read"}, scores: []float64{0.60}, wantTool: "read", wantScore: 0.60, wantRelevant: true},
		{name: "just below threshold is retained but not relevant", tools: []string{"read"}, scores: []float64{0.599}, wantTool: "read", wantScore: 0.599},
		{name: "selects strongest tool even when it appears later", tools: []string{"read", "edit", "bash"}, scores: []float64{0.42, 0.81, 0.66}, wantTool: "edit", wantScore: 0.81, wantRelevant: true},
		{name: "ties keep the first strongest tool", tools: []string{"read", "edit"}, scores: []float64{0.71, 0.71}, wantTool: "read", wantScore: 0.71, wantRelevant: true},
		{name: "negative similarities are not replaced by zero", tools: []string{"read", "edit"}, scores: []float64{-0.4, -0.2}, wantTool: "edit", wantScore: -0.2},
		{name: "no usable scores returns empty signal", tools: []string{"read"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := evidenceSignalFromScores(test.tools, test.scores)
			if got.Tool != test.wantTool || got.Similarity != test.wantScore || got.Relevant != test.wantRelevant {
				t.Fatalf("evidenceSignalFromScores() = %+v, want tool=%q similarity=%v relevant=%v", got, test.wantTool, test.wantScore, test.wantRelevant)
			}
		})
	}
}

func TestCosineThresholdSignalBoundary(t *testing.T) {
	score := cosineSimilarity([]float32{1, 0}, []float32{0.6, 0.8})
	signal := evidenceSignalFromScores([]string{"edit"}, []float64{score})
	if !signal.Relevant {
		t.Fatalf("cosine score %v at the configured boundary should be relevant", score)
	}
}

func TestRevalidationRequiresEvidenceOrUserConfirmation(t *testing.T) {
	err := RevalidateMemory(context.Background(), nil, 1, RevalidationDecision{})
	if err == nil || err.Error() != "revalidation requires an evidence note or explicit user confirmation" {
		t.Fatalf("RevalidateMemory() error = %v, want explicit attestation error", err)
	}
}

func TestUniqueSortedIDs(t *testing.T) {
	got := uniqueSortedIDs([]int{9, 2, 9, 1, 2})
	want := []int{1, 2, 9}
	if len(got) != len(want) {
		t.Fatalf("uniqueSortedIDs() = %v, want %v", got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("uniqueSortedIDs() = %v, want %v", got, want)
		}
	}
}
