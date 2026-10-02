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

func runZeroShotExtraction(t lemn.TurnPayload, pgDB *sql.DB) (lemn.ModelExtraction, error) {
	memoryWorthy, probability, globallyApplicable, err := decideMemoryWorthiness(t)
	if err != nil {
		return lemn.ModelExtraction{}, err
	}
	if !memoryWorthy {
		return lemn.ModelExtraction{
			MemoryWorthy: false,
			Type:         "none",
			Confidence:   probability,
		}, nil
	}

	scope := lemn.NormalizeScope(t.ProjectID)
	if globallyApplicable {
		scope = lemn.GlobalScope
	}
	var dependencyCandidates []lemn.DependencyCandidate
	if pgDB != nil {
		candidateCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		dependencyCandidates, err = lemn.ListDependencyCandidates(candidateCtx, pgDB, scope, 40)
		if err != nil {
			return lemn.ModelExtraction{}, fmt.Errorf("dependency candidate lookup failed: %w", err)
		}
	}
	dependencyJSON, _ := json.Marshal(dependencyCandidates)
	dependencyIDs := make([]int, len(dependencyCandidates))
	for i, candidate := range dependencyCandidates {
		dependencyIDs[i] = candidate.ID
	}
	prompt := fmt.Sprintf(`Analyze this software engineering conversation turn.
User: %s
Assistant: %s

Respond ONLY with JSON matching this format:

{"type": "decision|architecture|bug_fix|none", "summary": "brief summary", "confidence": 0.0-1.0, "depends_on": []}

- depends_on may contain only IDs from the dependency candidate list, and only when this new claim relies on them remaining true. Do not infer dependencies from topical similarity.

Summary rules:
- State the durable fact itself (e.g. "The router uses a 25m idle timeout"), not what happened in the turn.
- Use type none when the turn only describes the conversation (what the assistant explained, outlined or answered) or confirms memory IDs, without a new fact about the project or user.

Memory scope for this claim: %s
Authoritative memories eligible as dependencies (same scope or global):
%s`,
		t.UserMessage, t.AssistantResponse, scope, string(dependencyJSON))

	reqBody, _ := json.Marshal(map[string]any{
		"model": getenv("LEMN_EXTRACTION_MODEL", "qwen2.5-coder"),
		"messages": []map[string]string{
			{"role": "user", "content": prompt},
		},
		"response_format": map[string]string{"type": "json_object"},
	})

	client := http.Client{Timeout: getenvDuration("LEMN_EXTRACTION_TIMEOUT", 120*time.Second)}
	extractReq, err := http.NewRequest(http.MethodPost, getenv("LEMN_EXTRACTION_URL", "http://localhost:8000/v1/chat/completions"), bytes.NewBuffer(reqBody))
	if err != nil {
		return lemn.ModelExtraction{}, fmt.Errorf("failed to build extraction request: %w", err)
	}
	extractReq.Header.Set("Content-Type", "application/json")
	if key := os.Getenv("LEMN_BACKEND_API_KEY"); key != "" {
		extractReq.Header.Set("Authorization", "Bearer "+key)
	}

	resp, err := client.Do(extractReq)
	if err != nil {
		return lemn.ModelExtraction{}, fmt.Errorf("HTTP request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return lemn.ModelExtraction{}, fmt.Errorf("LLM API returned status %d", resp.StatusCode)
	}

	var result struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return lemn.ModelExtraction{}, fmt.Errorf("JSON decode error: %w", err)
	}
	if len(result.Choices) == 0 {
		return lemn.ModelExtraction{}, fmt.Errorf("empty choices array from LLM")
	}

	var payload struct {
		Type       string  `json:"type"`
		Summary    string  `json:"summary"`
		Confidence float64 `json:"confidence"`
		DependsOn  []int   `json:"depends_on"`
	}
	if err := json.Unmarshal([]byte(result.Choices[0].Message.Content), &payload); err != nil {
		return lemn.ModelExtraction{}, fmt.Errorf("invalid extraction JSON: %w", err)
	}

	// Type "none" is the extraction model's own veto: Laya thought the turn
	// was worth a look, but there was no durable claim in it to store.
	if payload.Type == "" || payload.Type == "none" || lemn.IsMetaSummary(payload.Summary) {
		return lemn.ModelExtraction{
			MemoryWorthy: false,
			Type:         "none",
			Confidence:   payload.Confidence,
		}, nil
	}

	relation := "independent"
	targetID := 0
	relationCandidates := make([]lemn.MatchTarget, 0)
	var summaryEmbedding []float32
	if pgDB != nil {
		searchCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		relationCandidates, summaryEmbedding, err = lemn.FindSimilarAuthoritative(searchCtx, pgDB, payload.Summary, scope)
		cancel()
		if err != nil {
			return lemn.ModelExtraction{}, fmt.Errorf("relation target search failed: %w", err)
		}
		if relationCandidates == nil {
			relationCandidates = make([]lemn.MatchTarget, 0)
		}
		if len(relationCandidates) > 0 {
			relation, targetID, err = classifyRelation(t, payload.Summary, relationCandidates)
			if err != nil {
				log.Printf("[Extraction] relation classification failed for turn %s; treating as independent: %v", t.ID, err)
				relation, targetID = "independent", 0
			}
		}
	}
	return lemn.ModelExtraction{
		MemoryWorthy:           true,
		GlobalScoped:           globallyApplicable,
		Type:                   payload.Type,
		Summary:                payload.Summary,
		Confidence:             payload.Confidence,
		GateProbability:        probability,
		Relation:               relation,
		TargetID:               targetID,
		DependsOn:              payload.DependsOn,
		DependencyCandidateIDs: dependencyIDs,
		RelationCandidates:     relationCandidates,
		SummaryEmbedding:       summaryEmbedding,
	}, nil
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
func decideMemoryWorthiness(t lemn.TurnPayload) (bool, float64, bool, error) {
	reqBody, _ := json.Marshal(map[string]string{
		"user_message":       t.UserMessage,
		"assistant_response": t.AssistantResponse,
	})

	req, err := http.NewRequest(http.MethodPost, layaMemoryURL, bytes.NewBuffer(reqBody))
	if err != nil {
		return false, 0, false, fmt.Errorf("failed to build memory-worthiness request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+sharedSecret)

	client := http.Client{Timeout: getenvDuration("LEMN_LAYA_TIMEOUT", 60*time.Second)}
	resp, err := client.Do(req)
	if err != nil {
		return false, 0, false, fmt.Errorf("memory-worthiness request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return false, 0, false, fmt.Errorf("memory-worthiness API returned status %d: %s", resp.StatusCode, string(body))
	}

	var result struct {
		MemoryWorthy       bool    `json:"memory_worthy"`
		Probability        float64 `json:"probability"`
		GloballyApplicable bool    `json:"globally_applicable"`
		GlobalProbability  float64 `json:"global_probability"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return false, 0, false, fmt.Errorf("memory-worthiness decode failed: %w", err)
	}

	if result.MemoryWorthy {
		log.Printf("[Gate] turn %s memory_worthy=true (p=%.2f) global=%t (p=%.2f)",
			t.ID, result.Probability, result.GloballyApplicable, result.GlobalProbability)
	}

	return result.MemoryWorthy, result.Probability, result.GloballyApplicable, nil
}
