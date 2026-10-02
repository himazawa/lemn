package lemn

import "testing"

func TestCosineSimilarity(t *testing.T) {
	tests := []struct {
		name string
		a    []float32
		b    []float32
		want float64
	}{
		{name: "identical vectors", a: []float32{1, 2, 3}, b: []float32{1, 2, 3}, want: 1},
		{name: "opposite vectors", a: []float32{1, 0}, b: []float32{-1, 0}, want: -1},
		{name: "orthogonal vectors", a: []float32{1, 0}, b: []float32{0, 1}, want: 0},
		{name: "scale invariant", a: []float32{1, 2}, b: []float32{10, 20}, want: 1},
		{name: "zero vector", a: []float32{0, 0}, b: []float32{1, 1}, want: 0},
		{name: "mismatched lengths", a: []float32{1, 2}, b: []float32{1, 2, 3}, want: 0},
		{name: "empty vectors", a: nil, b: nil, want: 0},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := cosineSimilarity(test.a, test.b)
			if diff := got - test.want; diff < -1e-9 || diff > 1e-9 {
				t.Fatalf("cosineSimilarity() = %v, want %v", got, test.want)
			}
		})
	}
}

func TestRetrievalThreshold(t *testing.T) {
	t.Run("default", func(t *testing.T) {
		t.Setenv("LEMN_RETRIEVAL_THRESHOLD", "")
		if got := retrievalThreshold(); got != 0.45 {
			t.Fatalf("retrievalThreshold() = %v, want 0.45", got)
		}
	})
	t.Run("env override", func(t *testing.T) {
		t.Setenv("LEMN_RETRIEVAL_THRESHOLD", "0.3")
		if got := retrievalThreshold(); got != 0.3 {
			t.Fatalf("retrievalThreshold() = %v, want 0.3", got)
		}
	})
	t.Run("invalid env falls back to default", func(t *testing.T) {
		t.Setenv("LEMN_RETRIEVAL_THRESHOLD", "high")
		if got := retrievalThreshold(); got != 0.45 {
			t.Fatalf("retrievalThreshold() = %v, want 0.45", got)
		}
	})
}

func TestGetenv(t *testing.T) {
	t.Run("set value wins", func(t *testing.T) {
		t.Setenv("LEMN_TEST_GETENV", "real")
		if got := getenv("LEMN_TEST_GETENV", "fallback"); got != "real" {
			t.Fatalf("getenv() = %q, want %q", got, "real")
		}
	})
	t.Run("unset returns fallback", func(t *testing.T) {
		if got := getenv("LEMN_TEST_GETENV_UNSET", "fallback"); got != "fallback" {
			t.Fatalf("getenv() = %q, want %q", got, "fallback")
		}
	})
	t.Run("empty value returns fallback", func(t *testing.T) {
		t.Setenv("LEMN_TEST_GETENV", "")
		if got := getenv("LEMN_TEST_GETENV", "fallback"); got != "fallback" {
			t.Fatalf("getenv() = %q, want %q", got, "fallback")
		}
	})
}
