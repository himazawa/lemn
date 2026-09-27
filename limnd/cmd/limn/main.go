package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strconv"
	"time"

	_ "github.com/lib/pq"

	"limnd/internal/limn"
)

func main() {
	dsn := os.Getenv("LIMN_POSTGRES_DSN")
	if dsn == "" {
		log.Fatal("LIMN_POSTGRES_DSN is not set — refusing to fall back to a default credential. " +
			"Set it to the same DSN used by the daemon.")
	}

	db, err := sql.Open("postgres", dsn)
	if err != nil {
		log.Fatalf("Failed to connect to Postgres kernel: %v", err)
	}
	defer db.Close()

	if len(os.Args) < 2 {
		printUsage()
		return
	}

	cmd := os.Args[1]
	switch cmd {
	case "pending":
		listPending(db)
	case "confirm":
		if len(os.Args) < 3 {
			fmt.Println("Usage: limn confirm <memory_id>")
			return
		}
		confirmMemory(db, os.Args[2])
	case "reject":
		if len(os.Args) < 3 {
			fmt.Println("Usage: limn reject <memory_id>")
			return
		}
		rejectMemory(db, os.Args[2])
	default:
		printUsage()
	}
}

func printUsage() {
	fmt.Println("LIMN Admin CLI")
	fmt.Println("  limn pending           - List all memories in PENDING or PENDING_CONFIRMATION state")
	fmt.Println("  limn confirm <id>      - Promote pending memory to AUTHORITATIVE (supersedes its linked target, if any)")
	fmt.Println("  limn reject <id>       - Transition pending memory to REJECTED")
}

func listPending(db *sql.DB) {
	rows, err := db.Query(`
		SELECT id, state, category, summary, confidence, provenance 
		FROM limn_memories 
		WHERE state IN ('PENDING', 'PENDING_CONFIRMATION') 
		ORDER BY created_at DESC;`)
	if err != nil {
		log.Fatalf("Failed to query pending memories: %v", err)
	}
	defer rows.Close()

	fmt.Println("\n--- PENDING MEMORIES FOR HUMAN REVIEW ---")
	count := 0
	for rows.Next() {
		var id int
		var state, category, summary string
		var confidence float64
		var provenanceJSON []byte

		rows.Scan(&id, &state, &category, &summary, &confidence, &provenanceJSON)
		count++

		fmt.Printf("\n[#%d] State: %s | Category: %s | Confidence: %.2f\n", id, state, category, confidence)
		fmt.Printf("      Summary: %s\n", summary)
		if len(provenanceJSON) > 0 {
			var prov map[string]interface{}
			json.Unmarshal(provenanceJSON, &prov)
			if targets, ok := prov["candidate_targets"]; ok {
				tJSON, _ := json.Marshal(targets)
				fmt.Printf("      Candidate Targets to Supersede: %s\n", string(tJSON))
			}
		}
	}
	if err := rows.Err(); err != nil {
		log.Fatalf("Failed while iterating pending memories: %v", err)
	}

	if count == 0 {
		fmt.Println("No pending memories requiring review.")
	}
}

func confirmMemory(db *sql.DB, idStr string) {
	id, err := strconv.Atoi(idStr)
	if err != nil {
		fmt.Printf("Invalid memory id %q: %v\n", idStr, err)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := limn.ConfirmPendingMemory(ctx, db, id); err != nil {
		log.Fatalf("Failed to confirm memory #%d: %v", id, err)
	}
	fmt.Printf("Memory #%d successfully promoted to AUTHORITATIVE (prior target superseded if one was linked).\n", id)
}

func rejectMemory(db *sql.DB, idStr string) {
	_, err := db.Exec("UPDATE limn_memories SET state = 'REJECTED' WHERE id = $1", idStr)
	if err != nil {
		log.Fatalf("Failed to reject memory #%s: %v", idStr, err)
	}
	fmt.Printf("Memory #%s marked as REJECTED.\n", idStr)
}
