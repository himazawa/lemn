package lemn

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"
)

func TestEmbeddingResponseValidation(t *testing.T) {
	for _, test := range []struct {
		name      string
		status    int
		body      string
		wantError bool
	}{
		{"backend unavailable", 507, `{"error":"memory exceeded"}`, true},
		{"empty data", 200, `{"data":[]}`, true},
		{"empty vector", 200, `{"data":[{"embedding":[]}]}`, true},
		{"valid vector", 200, `{"data":[{"embedding":[1,2]}]}`, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				writer.WriteHeader(test.status)
				writer.Write([]byte(test.body))
			}))
			defer server.Close()
			t.Setenv("LEMN_EMBEDDING_URL", server.URL)
			_, err := getEmbedding(context.Background(), "query")
			if (err != nil) != test.wantError {
				t.Fatalf("error = %v, wantError = %t", err, test.wantError)
			}
		})
	}
}

func TestEmbeddingBackendParity(t *testing.T) {
	oldURL, newURL := os.Getenv("LEMN_PARITY_OLD_URL"), os.Getenv("LEMN_PARITY_NEW_URL")
	if oldURL == "" || newURL == "" {
		t.Skip("requires both old and isolated embedding backends")
	}
	for _, text := range []string{"what is the value of y?", "The value of y is 2.", "HP framing consumes length minus four plus varint width.", "The project uses PostgreSQL."} {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		t.Setenv("LEMN_EMBEDDING_URL", oldURL)
		t.Setenv("LEMN_EMBEDDING_MODEL", os.Getenv("LEMN_PARITY_OLD_MODEL"))
		oldVector, err := getEmbedding(ctx, text)
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		t.Setenv("LEMN_EMBEDDING_URL", newURL)
		t.Setenv("LEMN_EMBEDDING_MODEL", "BAAI/bge-m3")
		newVector, err := getEmbedding(ctx, text)
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		similarity := cosineSimilarity(oldVector, newVector)
		if len(oldVector) != len(newVector) || similarity < 0.995 {
			t.Fatalf("incompatible vectors: old=%d new=%d cosine=%f", len(oldVector), len(newVector), similarity)
		}
	}
}

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
