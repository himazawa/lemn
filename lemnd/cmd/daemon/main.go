package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	_ "github.com/lib/pq"

	"lemnd/internal/authmw"
	"lemnd/internal/lemn"
	"lemnd/internal/logstore"
)

var (
	layaMemoryURL = getenv("LEMN_LAYA_MEMORY_URL", "http://localhost:8002/memory-worthiness")
	sharedSecret  string
)

func main() {
	sharedSecret = os.Getenv("LEMN_SHARED_SECRET")
	if sharedSecret == "" {
		log.Fatal("LEMN_SHARED_SECRET is not set — refusing to start unauthenticated. " +
			"Set it to a long random value and configure the same value in Pi and the router.")
	}

	bindAddr := getenv("LEMN_DAEMON_BIND", "127.0.0.1:8080")
	workerCount := getenvInt("LEMN_WORKER_COUNT", 2)

	sqliteDB, err := logstore.Open()
	if err != nil {
		log.Fatalf("Failed to open log store: %v", err)
	}
	defer sqliteDB.Close()

	if n, err := logstore.RecoverStuckJobs(sqliteDB); err != nil {
		log.Fatalf("Failed to recover stuck jobs: %v", err)
	} else if n > 0 {
		log.Printf("Recovered %d job(s) stuck in PROCESSING from a prior crash — requeued as RETRY", n)
	}

	var pgDB *sql.DB
	if dsn := os.Getenv("LEMN_POSTGRES_DSN"); dsn != "" {
		pgDB, err = sql.Open("postgres", dsn)
		if err != nil {
			log.Fatalf("Failed to open Postgres kernel DB: %v", err)
		}
		defer pgDB.Close()
		log.Println("Postgres kernel ENABLED — extractions will be routed through lemn.RouteExtraction")
	} else {
		log.Println("Postgres kernel DISABLED (no LEMN_POSTGRES_DSN) — bootstrap logging only")
	}

	// Start the durable job workers. Each worker polls memory_jobs for a
	// claimable row; SQLite's single-writer connection already serializes
	// their DB access, so it's safe to run several concurrently even
	// though only one can be mid-write at a time.
	stopCh := make(chan struct{})
	for i := 0; i < workerCount; i++ {
		go runWorker(i, sqliteDB, pgDB, stopCh)
	}
	log.Printf("Started %d job worker(s)", workerCount)

	http.HandleFunc("/log", authmw.Require(sharedSecret, handleLog(sqliteDB)))
	http.HandleFunc("/jobs/", authmw.Require(sharedSecret, handleJobStatus(sqliteDB)))
	http.HandleFunc("/retrieve", authmw.Require(sharedSecret, handleRetrieve(pgDB)))
	http.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	})

	fmt.Printf("LEMN Go daemon listening on %s...\n", bindAddr)
	log.Fatal(http.ListenAndServe(bindAddr, nil))
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func getenvInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

// Worker-side calls have no user waiting on them, so these are generous: a
// timeout here loses the memory entirely after its retries are exhausted.
func getenvDuration(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}

// handleLog durably enqueues the raw payload BEFORE responding. This is
// the fix for the old "go processTurn(...)" fire-and-forget pattern: if
// the process dies immediately after this handler returns 202, the turn
// is already safely in memory_jobs and will be recovered on restart.
func handleLog(sqliteDB *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		bodyBytes, err := readAll(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		var payload lemn.TurnPayload
		if err := json.Unmarshal(bodyBytes, &payload); err != nil {
			http.Error(w, "invalid turn payload: "+err.Error(), http.StatusBadRequest)
			return
		}
		if payload.ID == "" {
			http.Error(w, "turn payload missing id", http.StatusBadRequest)
			return
		}

		jobID, err := logstore.EnqueueJob(sqliteDB, payload.ID, bodyBytes)
		if err != nil {
			log.Printf("[Enqueue Error]: %v", err)
			http.Error(w, "failed to enqueue turn", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		json.NewEncoder(w).Encode(map[string]any{"job_id": jobID, "status": "QUEUED"})
	}
}

func handleRetrieve(pgDB *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if pgDB == nil {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode([]lemn.RetrievedMemory{})
			return
		}

		var req lemn.RetrievalRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()

		memories, err := lemn.QueryAuthoritativeMemories(ctx, pgDB, req.Query, req.ProjectID, req.Limit)
		if err != nil {
			log.Printf("[Retrieval Error]: %v", err)
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode([]lemn.RetrievedMemory{})
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(memories)
	}
}

func handleJobStatus(sqliteDB *sql.DB) http.HandlerFunc {
	type response struct {
		JobID      int64           `json:"job_id"`
		TurnID     string          `json:"turn_id"`
		Status     string          `json:"status"`
		Attempts   int             `json:"attempts"`
		Error      string          `json:"error,omitempty"`
		Extraction json.RawMessage `json:"extraction,omitempty"`
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		rawID := strings.TrimPrefix(r.URL.Path, "/jobs/")
		jobID, err := strconv.ParseInt(rawID, 10, 64)
		if err != nil || jobID <= 0 {
			http.Error(w, "invalid job id", http.StatusBadRequest)
			return
		}

		var result response
		var lastError, extraction sql.NullString
		err = sqliteDB.QueryRowContext(r.Context(), `
			SELECT j.id, j.turn_id, j.status, j.attempts, j.error, t.raw_llm_json
			FROM memory_jobs j LEFT JOIN turns t ON t.id = j.turn_id
			WHERE j.id = ?`, jobID,
		).Scan(&result.JobID, &result.TurnID, &result.Status, &result.Attempts, &lastError, &extraction)
		if err == sql.ErrNoRows {
			http.Error(w, "job not found", http.StatusNotFound)
			return
		}
		if err != nil {
			log.Printf("[Job Status Error]: %v", err)
			http.Error(w, "failed to query job status", http.StatusInternalServerError)
			return
		}
		if lastError.Valid {
			result.Error = lastError.String
		}
		if extraction.Valid && json.Valid([]byte(extraction.String)) {
			result.Extraction = json.RawMessage(extraction.String)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(result)
	}
}

func readAll(r *http.Request) ([]byte, error) {
	defer r.Body.Close()
	buf := new(bytes.Buffer)
	if _, err := buf.ReadFrom(r.Body); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// runWorker is the background loop that turns durable QUEUED/RETRY rows
// into actual extraction + kernel routing work. On any failure it lets
// logstore.MarkFailed decide retry-with-backoff vs permanent FAILED,
// rather than silently dropping the turn the way the old code did.
func runWorker(id int, sqliteDB, pgDB *sql.DB, stop <-chan struct{}) {
	for {
		select {
		case <-stop:
			return
		default:
		}

		job, err := logstore.ClaimNextJob(sqliteDB)
		if err == logstore.ErrNoJobAvailable {
			time.Sleep(500 * time.Millisecond)
			continue
		}
		if err != nil {
			log.Printf("[Worker %d] claim error: %v", id, err)
			time.Sleep(1 * time.Second)
			continue
		}

		if procErr := processJob(sqliteDB, pgDB, job); procErr != nil {
			log.Printf("[Worker %d] job %d (turn %s) failed: %v", id, job.ID, job.TurnID, procErr)
			if markErr := logstore.MarkFailed(sqliteDB, job, procErr); markErr != nil {
				log.Printf("[Worker %d] failed to record failure for job %d: %v", id, job.ID, markErr)
			}
			continue
		}

		if err := logstore.MarkCompleted(sqliteDB, job.ID); err != nil {
			log.Printf("[Worker %d] failed to mark job %d completed: %v", id, job.ID, err)
		}
	}
}

func processJob(sqliteDB, pgDB *sql.DB, job *logstore.Job) error {
	var t lemn.TurnPayload
	if err := json.Unmarshal(job.RawPayload, &t); err != nil {
		return fmt.Errorf("corrupt job payload: %w", err)
	}

	extraction, err := runZeroShotExtraction(t, pgDB)
	if err != nil {
		return fmt.Errorf("extraction failed: %w", err)
	}

	toolCallsJSON, _ := json.Marshal(t.ToolCalls)
	rawJSON, _ := json.Marshal(extraction)

	_, err = sqliteDB.Exec(`
		INSERT OR REPLACE INTO turns (id, user_message, assistant_response, tool_calls, is_memory_worthy, memory_type, extracted_summary, confidence, human_reviewed, raw_llm_json)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, FALSE, ?)`,
		t.ID, t.UserMessage, t.AssistantResponse, string(toolCallsJSON),
		extraction.MemoryWorthy, extraction.Type, extraction.Summary, extraction.Confidence, string(rawJSON),
	)
	if err != nil {
		return fmt.Errorf("sqlite write failed: %w", err)
	}

	if pgDB == nil {
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	memID, err := lemn.RouteExtraction(ctx, pgDB, t, extraction)
	if err != nil {
		return fmt.Errorf("kernel routing failed: %w", err)
	}
	if memID != 0 {
		log.Printf("Turn %s routed to memory #%d", t.ID, memID)
	}
	return nil
}

func rejectedByWorthinessGate(probability, globalProbability float64, globallyApplicable, explicitGlobal bool) lemn.ModelExtraction {
	return lemn.ModelExtraction{
		MemoryWorthy:        false,
		GatePassed:          false,
		GlobalScoped:        globallyApplicable,
		GlobalScopeExplicit: explicitGlobal,
		Type:                "none",
		GateProbability:     probability,
		GlobalProbability:   globalProbability,
	}
}

func rejectedByExtractor(gateProbability, globalProbability, extractionConfidence float64, globallyApplicable, explicitGlobal bool, reason string) lemn.ModelExtraction {
	return lemn.ModelExtraction{
		MemoryWorthy:        false,
		GatePassed:          true,
		GlobalScoped:        globallyApplicable,
		GlobalScopeExplicit: explicitGlobal,
		Type:                "none",
		Confidence:          extractionConfidence,
		GateProbability:     gateProbability,
		GlobalProbability:   globalProbability,
		ExtractorVetoReason: reason,
	}
}

func extractionVetoReason(extractionType, summary string) string {
	switch {
	case strings.TrimSpace(extractionType) == "":
		return "empty_type"
	case strings.EqualFold(strings.TrimSpace(extractionType), "none"):
		return "none_type"
	case lemn.IsMetaSummary(summary):
		return "meta_summary"
	default:
		return ""
	}
}

func extractionVetoReasonForTurn(extractionType, summary, userMessage string) string {
	if lemn.IsTransientInstruction(userMessage) {
		return "transient_instruction"
	}
	return extractionVetoReason(extractionType, summary)
}

func runZeroShotExtraction(t lemn.TurnPayload, pgDB *sql.DB) (lemn.ModelExtraction, error) {
	memoryWorthy, probability, globallyApplicable, globalProbability, err := decideMemoryWorthiness(t)
	if err != nil {
		return lemn.ModelExtraction{}, err
	}
	explicitGlobal := lemn.DetectExplicitGlobalPreference(t.UserMessage)
	globallyApplicable = globallyApplicable || explicitGlobal
	if !memoryWorthy {
		return rejectedByWorthinessGate(probability, globalProbability, globallyApplicable, explicitGlobal), nil
	}

	scope := lemn.NormalizeScope(t.ProjectID)
	if globallyApplicable {
		scope = lemn.GlobalScope
	}
	prompt := fmt.Sprintf(`Analyze this software engineering conversation turn.
User: %s
Assistant: %s

Respond ONLY with JSON matching this format:

{"type": "decision|architecture|bug_fix|preference|none", "summary": "brief summary", "confidence": 0.0-1.0}

Summary rules:
- Extract a claim from this turn only. Do not invent a fact or turn an assistant suggestion into a user decision.
- State the durable fact itself (e.g. "The router uses a 25m idle timeout"), not what happened in the turn.
- Use preference for a persistent user preference or interaction convention that should carry across future turns; preserve whether it is global or project-specific in the fact wording.
- A one-answer or one-task instruction (e.g. "for this answer only, be brief") is not a durable preference; use type none.
- Explicit durable user confirmations such as "Yes, adopt PostgreSQL going forward" establish a decision and should be extracted, even when phrased as a confirmation.
- Conversation acknowledgments such as "Yes, thanks", "Got it", or "That answers my question" do not establish a durable fact; use type none.
- Memory ID management such as "Confirm memory #12" or "Keep those memory IDs" is not a new project fact or user preference; use type none.
- Use type none when the turn only describes the conversation (what the assistant explained, outlined or answered), without a new fact about the project or user.

Memory scope for this claim: %s`,
		t.UserMessage, t.AssistantResponse, scope)

	var payload struct {
		Type       string  `json:"type"`
		Summary    string  `json:"summary"`
		Confidence float64 `json:"confidence"`
	}
	if err := requestExtractionJSON(prompt, &payload); err != nil {
		return lemn.ModelExtraction{}, fmt.Errorf("invalid extraction JSON: %w", err)
	}

	// Type "none" is the extraction model's own veto: Laya thought the turn
	// was worth a look, but there was no durable claim in it to store.
	vetoReason := extractionVetoReasonForTurn(payload.Type, payload.Summary, t.UserMessage)
	if vetoReason != "" {
		return rejectedByExtractor(probability, globalProbability, payload.Confidence, globallyApplicable, explicitGlobal, vetoReason), nil
	}

	var dependencyCandidates []lemn.DependencyCandidate
	if pgDB != nil {
		candidateCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		dependencyCandidates, err = lemn.ListDependencyCandidates(candidateCtx, pgDB, scope, 40)
		cancel()
		if err != nil {
			return lemn.ModelExtraction{}, fmt.Errorf("dependency candidate lookup failed: %w", err)
		}
	}
	dependencyIDs := make([]int, len(dependencyCandidates))
	for index, candidate := range dependencyCandidates {
		dependencyIDs[index] = candidate.ID
	}
	dependsOn, equivalent, err := resolveDependencies(payload.Type, payload.Summary, dependencyCandidates)
	if err != nil {
		return lemn.ModelExtraction{}, fmt.Errorf("dependency resolution failed: %w", err)
	}

	relation := "independent"
	targetID := 0
	relationCandidates := make([]lemn.MatchTarget, 0)
	var summaryEmbedding []float32
	if pgDB != nil && !equivalent {
		searchCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		relationCandidates, summaryEmbedding, err = lemn.FindSimilarAuthoritative(searchCtx, pgDB, payload.Summary, scope)
		cancel()
		if err != nil {
			return lemn.ModelExtraction{}, fmt.Errorf("relation target search failed: %w", err)
		}
		if relationCandidates == nil {
			relationCandidates = make([]lemn.MatchTarget, 0)
		}
		if len(relationCandidates) > 0 && !equivalent {
			relation, targetID, err = classifyRelation(t, payload.Summary, relationCandidates)
			if err != nil {
				return lemn.ModelExtraction{}, fmt.Errorf("relation classification failed for turn %s: %w", t.ID, err)
			}
		}
	}
	return lemn.ModelExtraction{
		MemoryWorthy:           true,
		ModelEquivalent:        equivalent,
		GatePassed:             true,
		GlobalScoped:           globallyApplicable,
		GlobalScopeExplicit:    explicitGlobal,
		GlobalProbability:      globalProbability,
		Type:                   payload.Type,
		Summary:                payload.Summary,
		Confidence:             payload.Confidence,
		GateProbability:        probability,
		Relation:               relation,
		TargetID:               targetID,
		DependsOn:              dependsOn,
		DependencyCandidateIDs: dependencyIDs,
		RelationCandidates:     relationCandidates,
		SummaryEmbedding:       summaryEmbedding,
	}, nil
}

func requestExtractionJSON(prompt string, payload any) error {
	body, err := json.Marshal(map[string]any{
		"model":           getenv("LEMN_EXTRACTION_MODEL", "qwen2.5-coder"),
		"messages":        []map[string]string{{"role": "user", "content": prompt}},
		"response_format": map[string]string{"type": "json_object"},
	})
	if err != nil {
		return fmt.Errorf("encode extraction request: %w", err)
	}
	request, err := http.NewRequest(http.MethodPost, getenv("LEMN_EXTRACTION_URL", "http://localhost:8000/v1/chat/completions"), bytes.NewBuffer(body))
	if err != nil {
		return fmt.Errorf("build extraction request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	if key := os.Getenv("LEMN_BACKEND_API_KEY"); key != "" {
		request.Header.Set("Authorization", "Bearer "+key)
	}
	client := http.Client{Timeout: getenvDuration("LEMN_EXTRACTION_TIMEOUT", 120*time.Second)}
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("extraction request failed: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("extraction API returned status %d", response.StatusCode)
	}
	var result struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		return fmt.Errorf("decode extraction response: %w", err)
	}
	if len(result.Choices) == 0 {
		return fmt.Errorf("empty extraction choices")
	}
	return json.Unmarshal([]byte(result.Choices[0].Message.Content), payload)
}

func resolveDependencies(extractionType, summary string, candidates []lemn.DependencyCandidate) ([]int, bool, error) {
	if len(candidates) == 0 {
		return []int{}, false, nil
	}
	claimJSON, err := json.Marshal(map[string]string{"type": extractionType, "summary": summary})
	if err != nil {
		return nil, false, err
	}
	candidatesJSON, err := json.Marshal(candidates)
	if err != nil {
		return nil, false, err
	}
	prompt := fmt.Sprintf(`Resolve dependencies for this already extracted, immutable claim:
%s

Do not copy candidate text into the claim or mutate/rewrite its summary or type. Do not extract another claim.
Choose only candidate IDs whose continued truth is required by the claim. Topical similarity is not a dependency.
If the claim is equivalent to or merely repeats an existing candidate fact, set equivalent to true and depends_on to [].
A repeated existing fact is not new independent evidence. Equivalence is not a dependency.
Otherwise set equivalent to false, and use [] if there are no actual dependencies.
Every dependency ID must be from the allowed candidates below. Return only dependency IDs and equivalence, never summary or type.

Allowed candidates:
%s

Respond only as JSON: {"depends_on":[],"equivalent":false}`, string(claimJSON), string(candidatesJSON))
	var result struct {
		DependsOn  json.RawMessage `json:"depends_on"`
		Equivalent *bool           `json:"equivalent"`
	}
	if err := requestExtractionJSON(prompt, &result); err != nil {
		return nil, false, err
	}
	var ids []int
	if err := json.Unmarshal(result.DependsOn, &ids); err != nil || ids == nil || result.Equivalent == nil {
		return nil, false, fmt.Errorf("resolver must return an integer depends_on array and boolean equivalent")
	}
	allowed := make(map[int]bool, len(candidates))
	for _, candidate := range candidates {
		allowed[candidate.ID] = true
	}
	seen := make(map[int]bool, len(ids))
	dependencies := make([]int, 0, len(ids))
	for _, id := range ids {
		if id <= 0 || !allowed[id] {
			return nil, false, fmt.Errorf("resolver returned disallowed dependency ID %d", id)
		}
		if !seen[id] {
			dependencies = append(dependencies, id)
			seen[id] = true
		}
	}
	if *result.Equivalent && len(dependencies) != 0 {
		return nil, false, fmt.Errorf("equivalent claims must have empty dependencies")
	}
	return dependencies, *result.Equivalent, nil
}

func classifyRelation(t lemn.TurnPayload, summary string, candidates []lemn.MatchTarget) (string, int, error) {
	candidatesJSON, err := json.Marshal(candidates)
	if err != nil {
		return "independent", 0, fmt.Errorf("encode relation candidates: %w", err)
	}
	prompt := fmt.Sprintf(`Decide whether this newly extracted fact changes an existing authoritative fact.

New fact: %s
Turn context:
User: %s
Assistant: %s

Choose independent unless the turn clearly changes or rejects one candidate.
Use supersedes when the new fact replaces an older or no-longer-current fact.
Use contradicts only when the turn establishes that a candidate was false.
Related topic alone is not enough. If none matches, choose independent and target_id 0.
If choosing a relation, target_id must be one of the candidate IDs below.

Candidates:
%s

Respond only as JSON: {"relation":"independent|supersedes|contradicts","target_id":0}`,
		summary, t.UserMessage, t.AssistantResponse, string(candidatesJSON))

	body, _ := json.Marshal(map[string]any{
		"model":           getenv("LEMN_EXTRACTION_MODEL", "qwen2.5-coder"),
		"messages":        []map[string]string{{"role": "user", "content": prompt}},
		"response_format": map[string]string{"type": "json_object"},
	})
	request, err := http.NewRequest(http.MethodPost, getenv("LEMN_EXTRACTION_URL", "http://localhost:8000/v1/chat/completions"), bytes.NewBuffer(body))
	if err != nil {
		return "independent", 0, fmt.Errorf("build relation request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	if key := os.Getenv("LEMN_BACKEND_API_KEY"); key != "" {
		request.Header.Set("Authorization", "Bearer "+key)
	}
	client := http.Client{Timeout: getenvDuration("LEMN_EXTRACTION_TIMEOUT", 120*time.Second)}
	response, err := client.Do(request)
	if err != nil {
		return "independent", 0, fmt.Errorf("relation request failed: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "independent", 0, fmt.Errorf("relation API returned status %d", response.StatusCode)
	}
	var result struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		return "independent", 0, fmt.Errorf("decode relation response: %w", err)
	}
	if len(result.Choices) == 0 {
		return "independent", 0, fmt.Errorf("empty relation choices")
	}
	var choice struct {
		Relation string `json:"relation"`
		TargetID int    `json:"target_id"`
	}
	if err := json.Unmarshal([]byte(result.Choices[0].Message.Content), &choice); err != nil {
		return "independent", 0, fmt.Errorf("invalid relation JSON: %w", err)
	}
	return validateRelationChoice(choice.Relation, choice.TargetID, candidates)
}

func validateRelationChoice(relation string, targetID int, candidates []lemn.MatchTarget) (string, int, error) {
	if relation == "independent" {
		return "independent", 0, nil
	}
	if relation != "supersedes" && relation != "contradicts" {
		return "independent", 0, nil
	}
	for _, candidate := range candidates {
		if candidate.ID == targetID {
			return relation, targetID, nil
		}
	}
	return "independent", 0, nil
}

// Returns memory-worthiness, its probability, and whether the turn is a
// user-level preference that should be stored in the global scope.
func decideMemoryWorthiness(t lemn.TurnPayload) (bool, float64, bool, float64, error) {
	reqBody, _ := json.Marshal(map[string]string{
		"user_message":       t.UserMessage,
		"assistant_response": t.AssistantResponse,
	})

	req, err := http.NewRequest(http.MethodPost, layaMemoryURL, bytes.NewBuffer(reqBody))
	if err != nil {
		return false, 0, false, 0, fmt.Errorf("failed to build memory-worthiness request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+sharedSecret)

	client := http.Client{Timeout: getenvDuration("LEMN_LAYA_TIMEOUT", 60*time.Second)}
	resp, err := client.Do(req)
	if err != nil {
		return false, 0, false, 0, fmt.Errorf("memory-worthiness request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return false, 0, false, 0, fmt.Errorf("memory-worthiness API returned status %d: %s", resp.StatusCode, string(body))
	}

	var result struct {
		MemoryWorthy       bool    `json:"memory_worthy"`
		Probability        float64 `json:"probability"`
		GloballyApplicable bool    `json:"globally_applicable"`
		GlobalProbability  float64 `json:"global_probability"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return false, 0, false, 0, fmt.Errorf("memory-worthiness decode failed: %w", err)
	}

	if result.MemoryWorthy {
		log.Printf("[Gate] turn %s memory_worthy=true (p=%.2f) global=%t (p=%.2f)",
			t.ID, result.Probability, result.GloballyApplicable, result.GlobalProbability)
	}

	return result.MemoryWorthy, result.Probability, result.GloballyApplicable, result.GlobalProbability, nil
}
