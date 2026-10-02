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

func TestDetectExplicitGlobalPreference(t *testing.T) {
	tests := []struct {
		name    string
		message string
		want    bool
	}{
		{name: "explicit across all projects preference", message: "Across all projects, I prefer concise answers.", want: true},
		{name: "preference in every project", message: "I prefer metric units in every project.", want: true},
		{name: "future convention across my projects", message: "For future code reviews across my projects, lead with risks first.", want: true},
		{name: "lasting preference no matter which project", message: "No matter which project we're in, use ISO dates; that's my lasting preference.", want: true},
		{name: "likes in any codebase", message: "Please remember I like examples first in any codebase.", want: true},
		{name: "whatever repository paraphrase", message: "I like short answers whatever repository I'm working in.", want: true},
		{name: "regardless of project paraphrase", message: "My preference is metric units regardless of the project.", want: true},
		{name: "future repos paraphrase", message: "For future work in any repo, I prefer examples before abstractions.", want: true},
		{name: "all future projects paraphrase", message: "I prefer a concise format for all my future projects.", want: true},
		{name: "across all my work paraphrase", message: "I usually want concise summaries across all my work.", want: true},
		{name: "project-specific convention is not global", message: "For this repository, use integration tests.", want: false},
		{name: "any project topic without preference is not global", message: "The service should be deployable to any project environment.", want: false},
		{name: "repository preference is local", message: "In this repository, I prefer table-driven unit tests.", want: false},
		{name: "single answer instruction is not global", message: "For this answer only, keep it brief.", want: false},
		{name: "temporary phrase dominates global cue", message: "Across all projects, for this one response only, use bullet points.", want: false},
		{name: "global topic without durable preference is not enough", message: "We changed all projects to use PostgreSQL.", want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := DetectExplicitGlobalPreference(test.message); got != test.want {
				t.Fatalf("DetectExplicitGlobalPreference(%q) = %v, want %v", test.message, got, test.want)
			}
		})
	}
}

func TestIsTransientInstruction(t *testing.T) {
	tests := []struct {
		message string
		want    bool
	}{
		{message: "For this answer only, keep it short.", want: true},
		{message: "Use this setting for this task only.", want: true},
		{message: "Just this time, skip the examples.", want: true},
		{message: "For this one response, use bullet points.", want: true},
		{message: "This one time, answer in French.", want: true},
		{message: "Across all projects, I prefer concise answers.", want: false},
		{message: "For future reviews, lead with risks.", want: false},
	}
	for _, test := range tests {
		if got := IsTransientInstruction(test.message); got != test.want {
			t.Errorf("IsTransientInstruction(%q) = %v, want %v", test.message, got, test.want)
		}
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
