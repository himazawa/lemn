package lemn

import "testing"

func TestSelectRetrievalResults(t *testing.T) {
	const threshold = 0.45
	const margin = 0.10 // fallback floor = 0.35

	mem := func(id int, sim float64) RetrievedMemory {
		return RetrievedMemory{ID: id, Category: "architecture", Summary: "s", Similarity: sim}
	}

	tests := []struct {
		name       string
		candidates []RetrievedMemory
		limit      int
		wantIDs    []int
		wantBelow  bool // every returned memory should carry this BelowThreshold flag
	}{
		{
			name:       "empty candidates",
			candidates: []RetrievedMemory{},
			limit:      5,
			wantIDs:    []int{},
		},
		{
			name:       "single match above threshold",
			candidates: []RetrievedMemory{mem(1, 0.55)},
			limit:      5,
			wantIDs:    []int{1},
			wantBelow:  false,
		},
		{
			name:       "multiple above threshold, ranked",
			candidates: []RetrievedMemory{mem(1, 0.6), mem(2, 0.5), mem(3, 0.46)},
			limit:      5,
			wantIDs:    []int{1, 2, 3},
			wantBelow:  false,
		},
		{
			name:       "mixed: only above-threshold returned",
			candidates: []RetrievedMemory{mem(1, 0.5), mem(2, 0.44), mem(3, 0.2)},
			limit:      5,
			wantIDs:    []int{1},
			wantBelow:  false,
		},
		{
			// Nothing above 0.45, but best (0.446) is within the 0.35 floor ->
			// the exact "what do you know about this project?" near-miss case.
			name:       "near miss triggers fallback",
			candidates: []RetrievedMemory{mem(1, 0.446), mem(2, 0.44), mem(3, 0.40)},
			limit:      5,
			wantIDs:    []int{1, 2, 3},
			wantBelow:  true,
		},
		{
			// Best (0.30) is below the 0.35 floor -> genuinely unrelated, no noise.
			name:       "far miss returns nothing",
			candidates: []RetrievedMemory{mem(1, 0.30), mem(2, 0.20)},
			limit:      5,
			wantIDs:    []int{},
		},
		{
			// Best exactly at the floor boundary (0.35) -> included.
			name:       "best at floor boundary falls back",
			candidates: []RetrievedMemory{mem(1, 0.35), mem(2, 0.34)},
			limit:      5,
			wantIDs:    []int{1, 2},
			wantBelow:  true,
		},
		{
			// limit caps the fallback result.
			name:       "fallback respects limit",
			candidates: []RetrievedMemory{mem(1, 0.44), mem(2, 0.43), mem(3, 0.42), mem(4, 0.41)},
			limit:      2,
			wantIDs:    []int{1, 2},
			wantBelow:  true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := selectRetrievalResults(test.candidates, threshold, margin, test.limit)
			if len(got) != len(test.wantIDs) {
				t.Fatalf("len = %d, want %d (got %+v)", len(got), len(test.wantIDs), got)
			}
			for i, wantID := range test.wantIDs {
				if got[i].ID != wantID {
					t.Fatalf("got[%d].ID = %d, want %d", i, got[i].ID, wantID)
				}
				if got[i].BelowThreshold != test.wantBelow {
					t.Fatalf("got[%d].BelowThreshold = %v, want %v", i, got[i].BelowThreshold, test.wantBelow)
				}
			}
		})
	}
}

func TestSelectRetrievalResultsZeroMarginDisablesFallback(t *testing.T) {
	// margin 0 -> floor == threshold. Nothing is above, and the best (0.44) is
	// below the threshold, so no fallback fires.
	got := selectRetrievalResults(
		[]RetrievedMemory{{ID: 1, Similarity: 0.44}},
		0.45, 0.0, 5,
	)
	if len(got) != 0 {
		t.Fatalf("len = %d, want 0 (fallback should be disabled at margin 0)", len(got))
	}
}

func TestRetrievalFallbackMargin(t *testing.T) {
	t.Run("default", func(t *testing.T) {
		t.Setenv("LEMN_RETRIEVAL_FALLBACK_MARGIN", "")
		if got := retrievalFallbackMargin(); got != 0.10 {
			t.Fatalf("retrievalFallbackMargin() = %v, want 0.10", got)
		}
	})
	t.Run("env override", func(t *testing.T) {
		t.Setenv("LEMN_RETRIEVAL_FALLBACK_MARGIN", "0.2")
		if got := retrievalFallbackMargin(); got != 0.2 {
			t.Fatalf("retrievalFallbackMargin() = %v, want 0.2", got)
		}
	})
	t.Run("invalid env falls back to default", func(t *testing.T) {
		t.Setenv("LEMN_RETRIEVAL_FALLBACK_MARGIN", "high")
		if got := retrievalFallbackMargin(); got != 0.10 {
			t.Fatalf("retrievalFallbackMargin() = %v, want 0.10", got)
		}
	})
}
