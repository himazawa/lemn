package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestStreamingBudgetReportedAfterUsage(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var payload struct {
			StreamOptions struct {
				IncludeUsage bool `json:"include_usage"`
			} `json:"stream_options"`
		}
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil || !payload.StreamOptions.IncludeUsage {
			t.Error("stream request must include usage")
		}
		writer.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(writer, "data: {\"choices\":[{\"delta\":{\"content\":\"answer\"},\"finish_reason\":null}]}\n\n")
		fmt.Fprint(writer, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		fmt.Fprint(writer, "data: {\"choices\":[],\"usage\":{\"completion_tokens\":8192}}\n\ndata: [DONE]\n\n")
	}))
	defer backend.Close()
	oldURL, oldName := heavyModelURL, heavyModelName
	heavyModelURL, heavyModelName = backend.URL, ""
	t.Cleanup(func() { heavyModelURL, heavyModelName = oldURL, oldName })
	request := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"messages":[],"stream":true,"max_tokens":8192}`))
	response := httptest.NewRecorder()
	handleChatCompletions(response, request)
	output := response.Body.String()
	if !strings.Contains(output, `"finish_reason":"length"`) || !strings.Contains(output, "answer") || strings.Contains(output, `"finish_reason":"stop"`) {
		t.Fatalf("invalid relayed stream: %s", output)
	}
	if strings.Index(output, `"length"`) > strings.Index(output, "[DONE]") {
		t.Fatal("finish must precede DONE")
	}
}

func TestMarkBudgetFinish(t *testing.T) {
	line := []byte("data: {\"choices\":[{\"delta\":{\"content\":\"last\"},\"finish_reason\":\"stop\"}]}\n")
	for _, test := range []struct {
		used, budget int
		truncated    bool
	}{
		{8192, 8192, true}, {8191, 8192, false}, {8192, 0, false},
	} {
		got := markBudgetFinish(line, test.used, test.budget)
		if bytes.Contains(got, []byte(`"length"`)) != test.truncated {
			t.Fatalf("unexpected finish: %s", got)
		}
		if !bytes.Contains(got, []byte(`"last"`)) {
			t.Fatalf("lost final content: %s", got)
		}
	}
}

func TestBackendTransportErrorDoesNotPanic(t *testing.T) {
	oldURL, oldName := heavyModelURL, heavyModelName
	heavyModelURL, heavyModelName = "http://127.0.0.1:1/v1/chat/completions", ""
	t.Cleanup(func() { heavyModelURL, heavyModelName = oldURL, oldName })
	request := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"messages":[]}`))
	response := httptest.NewRecorder()
	handleChatCompletions(response, request)
	if response.Code != 502 {
		t.Fatalf("status = %d, want 502", response.Code)
	}
}
