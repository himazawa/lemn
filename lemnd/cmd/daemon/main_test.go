package main

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"lemnd/internal/lemn"
	"lemnd/internal/logstore"
)

func TestHandleJobStatus(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "jobs.db")
	t.Setenv("LEMN_SQLITE_PATH", dbPath)
	db, err := logstore.Open()
	if err != nil {
		t.Fatalf("logstore.Open() error = %v", err)
	}
	defer db.Close()

	jobID, err := logstore.EnqueueJob(db, "bench-turn-1", []byte(`{"id":"bench-turn-1"}`))
	if err != nil {
		t.Fatalf("EnqueueJob() error = %v", err)
	}
	handler := handleJobStatus(db)

	t.Run("queued job status", func(t *testing.T) {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, "/jobs/1", nil)
		handler.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusOK, recorder.Body.String())
		}
		var body struct {
			Status     string          `json:"status"`
			Extraction json.RawMessage `json:"extraction"`
		}
		if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode response: %v", err)
		}
		if body.Status != "QUEUED" || len(body.Extraction) != 0 {
			t.Fatalf("response = %+v, want queued without extraction", body)
		}
	})

	t.Run("completed extraction result", func(t *testing.T) {
		if _, err := db.Exec(`INSERT INTO turns (id, raw_llm_json) VALUES (?, ?)`, "bench-turn-1", `{"memory_worthy":true,"summary":"kept fact"}`); err != nil {
			t.Fatalf("insert test turn: %v", err)
		}
		if _, err := db.Exec(`UPDATE memory_jobs SET status = 'COMPLETED' WHERE id = ?`, jobID); err != nil {
			t.Fatalf("complete test job: %v", err)
		}
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, "/jobs/1", nil)
		handler.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusOK, recorder.Body.String())
		}
		var body struct {
			Status     string `json:"status"`
			Extraction struct {
				MemoryWorthy bool   `json:"memory_worthy"`
				Summary      string `json:"summary"`
			} `json:"extraction"`
		}
		if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode response: %v", err)
		}
		if body.Status != "COMPLETED" || !body.Extraction.MemoryWorthy || body.Extraction.Summary != "kept fact" {
			t.Fatalf("response = %+v, want completed extraction", body)
		}
	})

	t.Run("invalid and unknown IDs", func(t *testing.T) {
		for path, wantStatus := range map[string]int{"/jobs/nope": http.StatusBadRequest, "/jobs/999": http.StatusNotFound} {
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
			if recorder.Code != wantStatus {
				t.Errorf("GET %s status = %d, want %d", path, recorder.Code, wantStatus)
			}
		}
	})
}

func TestExplicitRememberClaimIsDeterministic(t *testing.T) {
	for input, want := range map[string]string{
		"remember that the value of y is 2": "the value of y is 2",
		"Sorry, remember that y is y.":      "y is y",
		"remember use metric units":         "use metric units",
		"for this task, inspect HP":         "",
	} {
		if got := explicitRememberClaim(input); got != want {
			t.Errorf("explicitRememberClaim(%q)=%q, want %q", input, got, want)
		}
	}
}

func TestExplicitRememberBypassesFlakyClassifiers(t *testing.T) {
	mockExtractionServices(t, func(prompt string) (string, int) {
		t.Error("explicit declaration should not call an LLM extractor")
		return "", http.StatusInternalServerError
	}, false)
	got, err := runZeroShotExtraction(lemn.TurnPayload{ProjectID: "demo", UserMessage: "sorry remember that the value of y is 2"}, nil)
	if err != nil || !got.MemoryWorthy || !got.UserDeclared || got.GatePassed || got.Type != "decision" || got.Summary != "User-declared project fact: the value of y is 2" {
		t.Fatalf("extraction=%+v err=%v", got, err)
	}
}
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

func TestRejectionStagesRemainDistinct(t *testing.T) {
	gateReject := rejectedByWorthinessGate(0.21, 0.73, true, true)
	if gateReject.MemoryWorthy || gateReject.GatePassed || gateReject.GateProbability != 0.21 || gateReject.GlobalProbability != 0.73 || !gateReject.GlobalScoped || !gateReject.GlobalScopeExplicit || gateReject.Confidence != 0 {
		t.Fatalf("gate rejection = %+v, want gate/global probabilities preserved without extraction confidence", gateReject)
	}

	extractorVeto := rejectedByExtractor(0.72, 0.81, 0.93, true, true, "none_type")
	if extractorVeto.MemoryWorthy || !extractorVeto.GatePassed || extractorVeto.GateProbability != 0.72 || extractorVeto.GlobalProbability != 0.81 || !extractorVeto.GlobalScoped || !extractorVeto.GlobalScopeExplicit || extractorVeto.Confidence != 0.93 || extractorVeto.ExtractorVetoReason != "none_type" {
		t.Fatalf("extractor veto = %+v, want stage probabilities, confidence and veto reason preserved", extractorVeto)
	}
}

func TestExtractorVetoReason(t *testing.T) {
	tests := []struct {
		name    string
		typ     string
		summary string
		want    string
	}{
		{name: "empty type", want: "empty_type"},
		{name: "none type", typ: "none", want: "none_type"},
		{name: "meta summary", typ: "architecture", summary: "The assistant explains the project design.", want: "meta_summary"},
		{name: "durable claim", typ: "decision", summary: "The service uses PostgreSQL."},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := extractionVetoReason(test.typ, test.summary)
			if got != test.want {
				t.Fatalf("extractionVetoReason() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestTransientInstructionVetoTakesPrecedence(t *testing.T) {
	got := extractionVetoReasonForTurn(
		"preference",
		"The user prefers concise answers.",
		"For this answer only, keep it short.",
	)
	if got != "transient_instruction" {
		t.Fatalf("extractionVetoReasonForTurn() = %q, want transient_instruction", got)
	}
}

func TestTaskInstructionDoesNotVetoProjectFinding(t *testing.T) {
	if got := extractionVetoReasonForTurn("architecture", "HP framing consumes length minus four plus varint width.", "For this task only, inspect the failing test."); got != "" {
		t.Fatalf("project finding vetoed: %s", got)
	}
}

func TestGateRejectionReviewIsNarrow(t *testing.T) {
	for _, test := range []struct {
		turn lemn.TurnPayload
		want bool
	}{
		{lemn.TurnPayload{UserMessage: "sorry remember that y is 2"}, true},
		{lemn.TurnPayload{UserMessage: "thanks", AssistantResponse: "Understood."}, false},
		{lemn.TurnPayload{AssistantResponse: "Test passes", ToolCalls: []lemn.ToolCallEvidence{{Name: "bash", DiffText: "ok"}}}, true},
		{lemn.TurnPayload{AssistantResponse: "Compressed", ToolCalls: []lemn.ToolCallEvidence{{Name: "compress", DiffText: "ok"}}}, false},
	} {
		if got := shouldReviewGateRejection(test.turn); got != test.want {
			t.Fatalf("gate review = %t, want %t", got, test.want)
		}
	}
	bounded := boundExtractionText("head"+strings.Repeat("x", 20000)+"tail", 100)
	if !strings.HasPrefix(bounded, "head") || !strings.HasSuffix(bounded, "tail") || len(bounded) > 130 {
		t.Fatal("extraction text lost boundaries or exceeded budget")
	}
}

func TestRetrieveUnavailableReturnsServiceError(t *testing.T) {
	response := httptest.NewRecorder()
	handleRetrieve(nil).ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/retrieve", strings.NewReader(`{"query":"y"}`)))
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", response.Code)
	}
}

func TestExtractionPromptBoundsToolEvidence(t *testing.T) {
	turn := lemn.TurnPayload{ProjectID: "demo", UserMessage: "Continue the investigation.", AssistantResponse: "The HP test passes."}
	turn.ToolCalls = append(turn.ToolCalls, lemn.ToolCallEvidence{Name: "compress", DiffText: "historical-summary-sentinel"})
	for index := 0; index < 10; index++ {
		turn.ToolCalls = append(turn.ToolCalls, lemn.ToolCallEvidence{Name: "bash", DiffText: strings.Repeat("x", 3000)})
	}
	prompt := extractionPrompt(turn, "demo")
	if strings.Contains(prompt, "historical-summary-sentinel") || strings.Count(prompt, "[excerpt truncated]") != 4 || strings.Count(prompt, "x") > 8100 {
		t.Fatal("tool evidence was not filtered or bounded")
	}
	if !strings.Contains(prompt, "The HP test passes.") || !strings.Contains(prompt, "Memory scope: demo") {
		t.Fatal("turn or scope missing")
	}
}

func TestExtractionInputs(t *testing.T) {
	for _, test := range []struct {
		name    string
		turn    lemn.TurnPayload
		typ     string
		summary string
	}{
		{"explicit remember", lemn.TurnPayload{ProjectID: "demo", UserMessage: "sorry remember that the value of y is 2", AssistantResponse: "The value of y is 2. Next we will add fixtures."}, "decision", "The user declares that y is 2."},
		{"validated HP report", lemn.TurnPayload{ProjectID: "demo", UserMessage: "For this task only, investigate the test.", AssistantResponse: "The HP test passes; framing consumes value - 4 + varint width. No fix made.", ToolCalls: []lemn.ToolCallEvidence{{Name: "bash", DiffText: "test a_dummy_whose_spawn_was_missed_is_known_by_its_hp ... ok"}}}, "architecture", "HP framing consumes value - 4 + varint width; the missed-spawn HP test passes."},
	} {
		t.Run(test.name, func(t *testing.T) {
			prompts := mockExtractionServices(t, func(prompt string) (string, int) {
				if !strings.Contains(prompt, test.turn.UserMessage) || !strings.Contains(prompt, "user-declared project fact") || !strings.Contains(prompt, "validated project behavior") {
					t.Error("extraction contract omits source or durable fact rules")
				}
				body, _ := json.Marshal(map[string]any{"type": test.typ, "summary": test.summary, "confidence": 0.9})
				return string(body), http.StatusOK
			})
			got, err := runZeroShotExtraction(test.turn, nil)
			if test.name == "explicit remember" {
				if err != nil || !got.MemoryWorthy || !got.UserDeclared || got.GatePassed || got.Summary != "User-declared project fact: the value of y is 2" || len(*prompts) != 0 {
					t.Fatalf("explicit extraction = %+v, error = %v, model calls = %d", got, err, len(*prompts))
				}
				return
			}
			if err != nil || !got.MemoryWorthy || got.Type != test.typ || got.Summary != test.summary || got.GlobalScoped {
				t.Fatalf("extraction = %+v, error = %v", got, err)
			}
		})
	}
}

func TestExtractionLive(t *testing.T) {
	if os.Getenv("LEMN_LIVE_EXTRACTION_TEST") != "true" {
		t.Skip("requires an explicitly configured live extraction backend")
	}
	oldSecret := sharedSecret
	sharedSecret = os.Getenv("LEMN_SHARED_SECRET")
	t.Cleanup(func() { sharedSecret = oldSecret })
	for _, test := range []struct {
		name string
		turn lemn.TurnPayload
	}{
		{"remember", lemn.TurnPayload{ProjectID: "demo", UserMessage: "sorry remember that the value of y is 2", AssistantResponse: "The value of y is 2. Next we will add fixtures."}},
		{"HP framing", lemn.TurnPayload{ProjectID: "demo", UserMessage: "Continue the HP packet decoder investigation.", AssistantResponse: "The missed-spawn HP test passes. Framing consumes value - 4 + varint width: value 20 and width 1 consume 17 bytes. HP scanning runs over the whole buffer. No fix made.", ToolCalls: []lemn.ToolCallEvidence{{Name: "bash", DiffText: "test a_dummy_whose_spawn_was_missed_is_known_by_its_hp ... ok"}}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := runZeroShotExtraction(test.turn, nil)
			if err != nil || !got.MemoryWorthy || got.GlobalScoped || strings.TrimSpace(got.Summary) == "" {
				t.Fatalf("live extraction = %+v, error = %v", got, err)
			}
			if test.name == "remember" && (!strings.Contains(got.Summary, "y") || !strings.Contains(got.Summary, "2")) {
				t.Fatalf("explicit declaration lost: %q", got.Summary)
			}
			t.Logf("%s: %s", got.Type, got.Summary)
		})
	}
}

func TestGateFallbackPreservesClassifierDecision(t *testing.T) {
	for _, typ := range []string{"architecture", "none"} {
		t.Run(typ, func(t *testing.T) {
			mockExtractionServices(t, func(prompt string) (string, int) {
				return `{"type":"` + typ + `","summary":"Validated framing rule.","confidence":0.9}`, http.StatusOK
			}, false)
			got, err := runZeroShotExtraction(lemn.TurnPayload{UserMessage: "Continue.", AssistantResponse: "Test passes.", ToolCalls: []lemn.ToolCallEvidence{{Name: "bash", DiffText: "ok"}}}, nil)
			if err != nil || got.GatePassed || !got.GateFallback || got.MemoryWorthy != (typ != "none") {
				t.Fatalf("fallback = %+v, error = %v", got, err)
			}
		})
	}
}

func mockExtractionServices(t *testing.T, respond func(string) (string, int), gate ...bool) *[]string {
	t.Helper()
	prompts := []string{}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/gate" {
			worthy := len(gate) == 0 || gate[0]
			json.NewEncoder(writer).Encode(map[string]any{"memory_worthy": worthy, "probability": 0.9})
			return
		}
		if request.Method != http.MethodPost || request.Header.Get("Content-Type") != "application/json" || request.Header.Get("Authorization") != "Bearer test-backend-key" {
			t.Errorf("unexpected extraction request: %s, headers %v", request.Method, request.Header)
		}
		var body struct {
			Model    string `json:"model"`
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil || len(body.Messages) != 1 || body.Model != "test-extractor" {
			t.Errorf("invalid extraction request: %+v, error %v", body, err)
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		prompt := body.Messages[0].Content
		prompts = append(prompts, prompt)
		content, statusCode := respond(prompt)
		writer.WriteHeader(statusCode)
		json.NewEncoder(writer).Encode(map[string]any{
			"choices": []any{map[string]any{"message": map[string]string{"content": content}}},
		})
	}))
	t.Cleanup(server.Close)
	oldURL := layaMemoryURL
	layaMemoryURL = server.URL + "/gate"
	t.Cleanup(func() { layaMemoryURL = oldURL })
	t.Setenv("LEMN_EXTRACTION_URL", server.URL+"/extract")
	t.Setenv("LEMN_EXTRACTION_MODEL", "test-extractor")
	t.Setenv("LEMN_BACKEND_API_KEY", "test-backend-key")
	return &prompts
}

func dependencyFixture(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(`CREATE TABLE lemn_memories (id INTEGER, project_id TEXT, summary TEXT, state TEXT, created_at INTEGER);
		INSERT INTO lemn_memories VALUES (12, 'alpha', 'Existing candidate sentinel: the billing service uses PostgreSQL.', 'AUTHORITATIVE', 1)`); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestInitialExtractionIsTurnOnlyAndVetoPrecedesCandidateLookup(t *testing.T) {
	prompts := mockExtractionServices(t, func(prompt string) (string, int) {
		for _, forbidden := range []string{"Existing candidate sentinel", "depends_on", "Allowed candidates", "Authoritative memories eligible"} {
			if strings.Contains(prompt, forbidden) {
				t.Errorf("initial prompt includes %q", forbidden)
			}
		}
		return `{"type":"none","summary":"","confidence":0.95}`, http.StatusOK
	})
	db := dependencyFixture(t)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	extraction, err := runZeroShotExtraction(lemn.TurnPayload{ProjectID: "alpha", UserMessage: "Yes, thanks."}, db)
	if err != nil || extraction.MemoryWorthy || extraction.ExtractorVetoReason != "none_type" || len(*prompts) != 1 {
		t.Fatalf("extraction = %+v, error = %v, calls = %d; want veto before closed DB lookup", extraction, err, len(*prompts))
	}
}

func TestDependencyResolutionInExtraction(t *testing.T) {
	tests := []struct {
		name       string
		response   string
		statusCode int
		wantError  bool
	}{
		{name: "repeated fact retained for review", response: `{"depends_on":[],"equivalent":true}`, statusCode: http.StatusOK},
		{name: "unknown ID fails closed", response: `{"depends_on":[99],"equivalent":false}`, statusCode: http.StatusOK, wantError: true},
		{name: "zero ID fails closed", response: `{"depends_on":[0],"equivalent":false}`, statusCode: http.StatusOK, wantError: true},
		{name: "string IDs fail closed", response: `{"depends_on":["12"],"equivalent":false}`, statusCode: http.StatusOK, wantError: true},
		{name: "malformed dependencies fail closed", response: `{"depends_on":"none","equivalent":false}`, statusCode: http.StatusOK, wantError: true},
		{name: "missing dependencies fail closed", response: `{"equivalent":false}`, statusCode: http.StatusOK, wantError: true},
		{name: "missing equivalence fails closed", response: `{"depends_on":[]}`, statusCode: http.StatusOK, wantError: true},
		{name: "null dependencies fail closed", response: `{"depends_on":null,"equivalent":false}`, statusCode: http.StatusOK, wantError: true},
		{name: "equivalence cannot depend on itself", response: `{"depends_on":[12],"equivalent":true}`, statusCode: http.StatusOK, wantError: true},
		{name: "invalid JSON fails closed", response: `not JSON`, statusCode: http.StatusOK, wantError: true},
		{name: "HTTP failure fails closed", statusCode: http.StatusServiceUnavailable, wantError: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			prompts := mockExtractionServices(t, func(prompt string) (string, int) {
				if strings.HasPrefix(prompt, "Analyze this") {
					if strings.Contains(prompt, "Existing candidate sentinel") || strings.Contains(prompt, "depends_on") {
						t.Error("initial extractor saw candidate data or dependency schema")
					}
					return `{"type":"decision","summary":"The billing service uses PostgreSQL.","confidence":0.95,"depends_on":[99]}`, http.StatusOK
				}
				for _, required := range []string{"immutable claim", "Existing candidate sentinel", "Do not copy", "mutate/rewrite", "Equivalence is not a dependency"} {
					if !strings.Contains(prompt, required) {
						t.Errorf("resolver prompt missing %q", required)
					}
				}
				return test.response, test.statusCode
			})
			extraction, err := runZeroShotExtraction(lemn.TurnPayload{ProjectID: "alpha", UserMessage: "The billing service uses PostgreSQL."}, dependencyFixture(t))
			if len(*prompts) != 2 {
				t.Fatalf("model calls = %d, want extraction then resolver", len(*prompts))
			}
			if test.wantError {
				if err == nil || !strings.Contains(err.Error(), "dependency resolution failed") || extraction.MemoryWorthy || extraction.Relation == "independent" {
					t.Fatalf("extraction = %+v, error = %v; want error without independent fallback", extraction, err)
				}
				return
			}
			if err != nil || !extraction.MemoryWorthy || !extraction.ModelEquivalent || extraction.Summary != "The billing service uses PostgreSQL." || len(extraction.DependsOn) != 0 {
				t.Fatalf("extraction = %+v, error = %v; want repeated fact preserved for review", extraction, err)
			}
		})
	}
}

func TestDependencyResolverCannotRewriteClaim(t *testing.T) {
	claim := lemn.ModelExtraction{Type: "decision", Summary: "The billing service uses PostgreSQL.", Confidence: 0.91}
	original := claim
	mockExtractionServices(t, func(prompt string) (string, int) {
		if !strings.Contains(prompt, claim.Summary) || !strings.Contains(prompt, `"type":"decision"`) {
			t.Error("resolver did not receive the original claim")
		}
		return `{"depends_on":[12,12],"equivalent":false,"summary":"Copied candidate text","type":"architecture","confidence":1}`, http.StatusOK
	})
	dependencies, equivalent, err := resolveDependencies(claim.Type, claim.Summary, []lemn.DependencyCandidate{{ID: 12, Summary: "Billing requires durable transactions."}})
	if err != nil || equivalent || !reflect.DeepEqual(dependencies, []int{12}) || !reflect.DeepEqual(claim, original) {
		t.Fatalf("dependencies = %v, equivalent = %t, error = %v, claim = %+v", dependencies, equivalent, err, claim)
	}
}

func TestInitialExtractionIgnoresDependencyHints(t *testing.T) {
	prompts := mockExtractionServices(t, func(prompt string) (string, int) {
		return `{"type":"decision","summary":"The billing service uses PostgreSQL.","confidence":0.91,"depends_on":[99]}`, http.StatusOK
	})
	extraction, err := runZeroShotExtraction(lemn.TurnPayload{UserMessage: "Yes, adopt PostgreSQL going forward."}, nil)
	if err != nil || !extraction.MemoryWorthy || extraction.Type != "decision" || extraction.Summary != "The billing service uses PostgreSQL." || extraction.Confidence != 0.91 || len(extraction.DependsOn) != 0 || len(*prompts) != 1 {
		t.Fatalf("extraction = %+v, error = %v; initial dependency hints must not be used", extraction, err)
	}
}

func TestAcknowledgmentsAndDurableConfirmationsWithMockedModel(t *testing.T) {
	tests := []struct {
		name    string
		user    string
		content string
		worthy  bool
	}{
		{name: "acknowledgment", user: "Yes, thanks.", content: `{"type":"none","summary":"","confidence":0.95}`},
		{name: "conversation acknowledgment", user: "Got it, that answers my question.", content: `{"type":"none","summary":"","confidence":0.95}`},
		{name: "memory ID management", user: "Confirm memory #12.", content: `{"type":"none","summary":"","confidence":0.95}`},
		{name: "durable confirmation", user: "Yes, adopt PostgreSQL going forward.", content: `{"type":"decision","summary":"The project adopts PostgreSQL going forward.","confidence":0.95}`, worthy: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			prompts := mockExtractionServices(t, func(prompt string) (string, int) {
				for _, required := range []string{test.user, "Explicit durable user confirmations", "Yes, adopt PostgreSQL going forward", "Conversation acknowledgments", "Memory ID management"} {
					if !strings.Contains(prompt, required) {
						t.Errorf("initial prompt missing %q", required)
					}
				}
				return test.content, http.StatusOK
			})
			extraction, err := runZeroShotExtraction(lemn.TurnPayload{UserMessage: test.user, AssistantResponse: "Understood."}, nil)
			if err != nil || extraction.MemoryWorthy != test.worthy || !extraction.GatePassed || len(*prompts) != 1 {
				t.Fatalf("extraction = %+v, error = %v, calls = %d", extraction, err, len(*prompts))
			}
			if test.worthy && (extraction.Type != "decision" || extraction.Summary != "The project adopts PostgreSQL going forward.") {
				t.Fatalf("durable claim changed: %+v", extraction)
			}
		})
	}
}
