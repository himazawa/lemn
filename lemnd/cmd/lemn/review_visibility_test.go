package main

import (
	"strings"
	"testing"
	"time"
)

func TestReviewAge(t *testing.T) {
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name string
		age  time.Duration
		want string
	}{
		{"recent", 26 * time.Hour, "1d 2h"},
		{"boundary", 7 * 24 * time.Hour, "7d 0h"},
		{"escalated", 7*24*time.Hour + time.Second, "7d 0h | NEEDS ATTENTION (>7 days)"},
		{"negative clamp", -time.Hour, "0d 0h"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := reviewAge(now.Add(-test.age), now); got != test.want {
				t.Fatalf("reviewAge = %q, want %q", got, test.want)
			}
		})
	}
}

func TestFormatReviewDependencies(t *testing.T) {
	for _, test := range []struct {
		name, raw, scope, want string
	}{
		{"edge", `[{"id":2,"source":"edge","state":"AUTHORITATIVE","project_id":"alpha"}]`, "alpha", "depends_on (edge): 2 | State: AUTHORITATIVE | Project: alpha"},
		{"global visible", `[{"id":2,"source":"provenance","state":"AUTHORITATIVE","project_id":"global"}]`, "alpha", "depends_on (provenance): 2 | State: AUTHORITATIVE | Project: global"},
		{"state and scope", `[{"id":2,"source":"edge","state":"REJECTED","project_id":"beta"}]`, "alpha", "BLOCKED: invalid state: REJECTED; invalid scope: beta"},
		{"global cannot depend on project", `[{"id":2,"source":"edge","state":"AUTHORITATIVE","project_id":"alpha"}]`, "global", "BLOCKED: invalid scope: alpha"},
		{"missing", `[{"id":99,"source":"provenance","state":null,"project_id":null}]`, "alpha", "BLOCKED: dependency ID not found"},
		{"malformed", `[{"id":"bad","source":"provenance","state":null,"project_id":null}]`, "alpha", "BLOCKED: invalid dependency ID"},
		{"nonpositive", `[{"id":0,"source":"provenance","state":null,"project_id":null}]`, "alpha", "BLOCKED: invalid dependency ID"},
		{"self", `[{"id":1,"source":"edge","state":"AUTHORITATIVE","project_id":"alpha"}]`, "alpha", "BLOCKED: invalid dependency ID (self or relation target)"},
		{"relation target", `[{"id":3,"source":"provenance","state":"AUTHORITATIVE","project_id":"alpha"}]`, "alpha", "BLOCKED: invalid dependency ID (self or relation target)"},
	} {
		t.Run(test.name, func(t *testing.T) {
			lines, err := formatReviewDependencies([]byte(test.raw), 1, 3, test.scope)
			if err != nil || len(lines) != 1 || !strings.Contains(lines[0], test.want) {
				t.Fatalf("formatReviewDependencies = %v, %v; want %q", lines, err, test.want)
			}
			if !strings.Contains(test.want, "BLOCKED") && strings.Contains(lines[0], "BLOCKED") {
				t.Fatalf("valid dependency marked blocked: %s", lines[0])
			}
		})
	}
	if _, err := formatReviewDependencies([]byte(`[{`), 1, 0, "alpha"); err == nil {
		t.Fatal("malformed dependency JSON was ignored")
	}
}
