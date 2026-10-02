package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
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
	gateReject := rejectedByWorthinessGate(0.21, 0.73, true)
	if gateReject.MemoryWorthy || gateReject.GatePassed || gateReject.GateProbability != 0.21 || gateReject.GlobalProbability != 0.73 || !gateReject.GlobalScoped || gateReject.Confidence != 0 {
		t.Fatalf("gate rejection = %+v, want gate/global probabilities preserved without extraction confidence", gateReject)
	}

	extractorVeto := rejectedByExtractor(0.72, 0.81, 0.93, true)
	if extractorVeto.MemoryWorthy || !extractorVeto.GatePassed || extractorVeto.GateProbability != 0.72 || extractorVeto.GlobalProbability != 0.81 || !extractorVeto.GlobalScoped || extractorVeto.Confidence != 0.93 {
		t.Fatalf("extractor veto = %+v, want gate/global predictions and extraction confidence preserved", extractorVeto)
	}
}

func TestParseDependencyIDs(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    []int
		wantErr bool
	}{
		{name: "integer array", input: `[2,4]`, want: []int{2, 4}},
		{name: "numeric string array", input: `["2","4"]`, want: []int{2, 4}},
		{name: "empty string means none", input: `"none"`},
		{name: "encoded empty array means none", input: `"[]"`},
		{name: "single numeric string", input: `"7"`, want: []int{7}},
		{name: "malformed optional hint", input: `"current project"`, wantErr: true},
		{name: "non-array object", input: `{"id":2}`, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := parseDependencyIDs(json.RawMessage(test.input))
			if (err != nil) != test.wantErr {
				t.Fatalf("parseDependencyIDs() error = %v, wantErr %v", err, test.wantErr)
			}
			if len(got) != len(test.want) {
				t.Fatalf("parseDependencyIDs() = %v, want %v", got, test.want)
			}
			for i := range got {
				if got[i] != test.want[i] {
					t.Fatalf("parseDependencyIDs() = %v, want %v", got, test.want)
				}
			}
		})
	}
}
