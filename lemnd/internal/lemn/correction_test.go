package lemn

import "testing"

func TestNormalizeScope(t *testing.T) {
	tests := []struct {
		projectID string
		want      string
	}{
		{projectID: "", want: GlobalScope},
		{projectID: "my-project", want: "my-project"},
		{projectID: GlobalScope, want: GlobalScope},
	}

	for _, test := range tests {
		if got := NormalizeScope(test.projectID); got != test.want {
			t.Fatalf("NormalizeScope(%q) = %q, want %q", test.projectID, got, test.want)
		}
	}
}

func TestDetectCorrectionIntent(t *testing.T) {
	tests := []struct {
		name     string
		message  string
		override bool
	}{
		{name: "plain question", message: "how does the router pick a model?", override: false},
		{name: "forget", message: "forget what I said about the cache size", override: true},
		{name: "no longer, case-insensitive", message: "We NO LONGER use webpack", override: true},
		{name: "instead of", message: "use pnpm instead of npm from now on", override: true},
		{name: "migrate from", message: "migrate from postgres to cockroach", override: true},
		{name: "past tense does not match the current regex", message: "we migrated from postgres to cockroach", override: false},
		{name: "negated replace is not an override", message: "don't replace the existing config", override: false},
		{name: "negated supersede is not an override", message: "this won't supersede the older decision", override: false},
		{name: "imperative supersede", message: "supersede the draft with the final version", override: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := DetectCorrectionIntent(test.message)
			if got.IsExplicitOverride != test.override {
				t.Fatalf("DetectCorrectionIntent(%q).IsExplicitOverride = %v, want %v", test.message, got.IsExplicitOverride, test.override)
			}
			if got.UserMessage != test.message {
				t.Fatalf("DetectCorrectionIntent() did not preserve the message")
			}
		})
	}
}

func TestSupersedeThreshold(t *testing.T) {
	t.Run("default", func(t *testing.T) {
		t.Setenv("LEMN_SUPERSEDE_THRESHOLD", "")
		if got := supersedeThreshold(); got != correctionMatchThreshold {
			t.Fatalf("supersedeThreshold() = %v, want %v", got, correctionMatchThreshold)
		}
	})
	t.Run("env override", func(t *testing.T) {
		t.Setenv("LEMN_SUPERSEDE_THRESHOLD", "0.88")
		if got := supersedeThreshold(); got != 0.88 {
			t.Fatalf("supersedeThreshold() = %v, want 0.88", got)
		}
	})
	t.Run("invalid env falls back to default", func(t *testing.T) {
		t.Setenv("LEMN_SUPERSEDE_THRESHOLD", "not-a-number")
		if got := supersedeThreshold(); got != correctionMatchThreshold {
			t.Fatalf("supersedeThreshold() = %v, want %v", got, correctionMatchThreshold)
		}
	})
}
