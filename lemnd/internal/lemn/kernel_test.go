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
			name:  "json array of numbers", value: []interface{}{float64(2), float64(4)},
			want:  []int{2, 4},
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
