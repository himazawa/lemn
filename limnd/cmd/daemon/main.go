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

	"limnd/internal/authmw"
	"limnd/internal/limn"
	"limnd/internal/logstore"
)

var (
	layaMemoryURL = getenv("LIMN_LAYA_MEMORY_URL", "http://localhost:8002/memory-worthiness")
	sharedSecret  string
)

func main() {
	sharedSecret = os.Getenv("LIMN_SHARED_SECRET")
	if sharedSecret == "" {
		log.Fatal("LIMN_SHARED_SECRET is not set — refusing to start unauthenticated. " +
			"Set it to a long random value and configure the same value in Pi and the router.")
	}

	bindAddr := getenv("LIMN_DAEMON_BIND", "127.0.0.1:8080")
	workerCount := getenvInt("LIMN_WORKER_COUNT", 2)

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
	if dsn := os.Getenv("LIMN_POSTGRES_DSN"); dsn != "" {
		pgDB, err = sql.Open("postgres", dsn)
		if err != nil {
			log.Fatalf("Failed to open Postgres kernel DB: %v", err)
		}
		defer pgDB.Close()
		log.Println("Postgres kernel ENABLED — extractions will be routed through limn.RouteExtraction")
	} else {
		log.Println("Postgres kernel DISABLED (no LIMN_POSTGRES_DSN) — bootstrap logging only")
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

	fmt.Printf("LIMN Go daemon listening on %s...\n", bindAddr)
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

		var payload limn.TurnPayload
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
			json.NewEncoder(w).Encode([]limn.RetrievedMemory{})
			return
		}

		var req limn.RetrievalRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()

		memories, err := limn.QueryAuthoritativeMemories(ctx, pgDB, req.Query, req.Limit)
		if err != nil {
			log.Printf("[Retrieval Error]: %v", err)
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode([]limn.RetrievedMemory{})
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
	var t limn.TurnPayload
	if err := json.Unmarshal(job.RawPayload, &t); err != nil {
		return fmt.Errorf("corrupt job payload: %w", err)
	}

	extraction, err := runZeroShotExtraction(t)
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

	memID, err := limn.RouteExtraction(ctx, pgDB, t, extraction)
	if err != nil {
		return fmt.Errorf("kernel routing failed: %w", err)
	}
	if memID != 0 {
		log.Printf("Turn %s routed to memory #%d", t.ID, memID)
	}
	return nil
}

func runZeroShotExtraction(t limn.TurnPayload) (limn.ModelExtraction, error) {
	memoryWorthy, probability, err := decideMemoryWorthiness(t)
	if err != nil {
		return limn.ModelExtraction{}, err
	}
	if !memoryWorthy {
		return limn.ModelExtraction{
			MemoryWorthy: false,
			Type:         "none",
			Confidence:   probability,
		}, nil
	}

	prompt := fmt.Sprintf(`Analyze this software engineering conversation turn.
User: %s
Assistant: %s

Respond ONLY with JSON matching this format:

{"type": "decision|architecture|bug_fix|none", "summary": "brief summary", "confidence": 0.0-1.0}`,
		t.UserMessage, t.AssistantResponse)

	reqBody, _ := json.Marshal(map[string]any{
		"model": "qwen2.5-coder",
		"messages": []map[string]string{
			{"role": "user", "content": prompt},
		},
		"response_format": map[string]string{"type": "json_object"},
	})

	client := http.Client{Timeout: 10 * time.Second}
	resp, err := client.Post("http://localhost:8000/v1/chat/completions", "application/json", bytes.NewBuffer(reqBody))
	if err != nil {
		return limn.ModelExtraction{}, fmt.Errorf("HTTP request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return limn.ModelExtraction{}, fmt.Errorf("LLM API returned status %d", resp.StatusCode)
	}

	var result struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return limn.ModelExtraction{}, fmt.Errorf("JSON decode error: %w", err)
	}
	if len(result.Choices) == 0 {
		return limn.ModelExtraction{}, fmt.Errorf("empty choices array from LLM")
	}

	var payload struct {
		Type       string  `json:"type"`
		Summary    string  `json:"summary"`
		Confidence float64 `json:"confidence"`
	}
	if err := json.Unmarshal([]byte(result.Choices[0].Message.Content), &payload); err != nil {
		return limn.ModelExtraction{}, fmt.Errorf("invalid extraction JSON: %w", err)
	}

	return limn.ModelExtraction{
		MemoryWorthy: true,
		Type:         payload.Type,
		Summary:      payload.Summary,
		Confidence:   payload.Confidence,
	}, nil
}

func decideMemoryWorthiness(t limn.TurnPayload) (bool, float64, error) {
	reqBody, _ := json.Marshal(map[string]string{
		"user_message":       t.UserMessage,
		"assistant_response": t.AssistantResponse,
	})

	req, err := http.NewRequest(http.MethodPost, layaMemoryURL, bytes.NewBuffer(reqBody))
	if err != nil {
		return false, 0, fmt.Errorf("failed to build memory-worthiness request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+sharedSecret)

	client := http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return false, 0, fmt.Errorf("memory-worthiness request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return false, 0, fmt.Errorf("memory-worthiness API returned status %d: %s", resp.StatusCode, string(body))
	}

	var result struct {
		MemoryWorthy bool    `json:"memory_worthy"`
		Probability  float64 `json:"probability"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return false, 0, fmt.Errorf("memory-worthiness decode failed: %w", err)
	}

	return result.MemoryWorthy, result.Probability, nil
}
