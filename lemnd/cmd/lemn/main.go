package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	_ "github.com/lib/pq"

	"lemnd/internal/lemn"
)

func main() {
	dsn := os.Getenv("LEMN_POSTGRES_DSN")
	if dsn == "" {
		log.Fatal("LEMN_POSTGRES_DSN is not set — refusing to fall back to a default credential. " +
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
			fmt.Println("Usage: lemn confirm <memory_id> [memory_id...]")
			return
		}
		confirmMemories(db, os.Args[2:])
	case "revalidate":
		if len(os.Args) < 3 {
			fmt.Println("Usage: lemn revalidate [--evidence-quote passage --evidence-source https_url | --user-confirmed] [--summary revised_summary] [--depends-on id,id | --clear-dependencies] <memory_id>")
			return
		}
		revalidateMemory(db, os.Args[2:])
	case "reject":
		if len(os.Args) < 3 {
			fmt.Println("Usage: lemn reject <memory_id> [memory_id...]")
			return
		}
		rejectMemories(db, os.Args[2:])
	case "sweep":
		sweepPending(db, len(os.Args) > 2 && os.Args[2] == "--apply")
	default:
		printUsage()
	}
}

func printUsage() {
	fmt.Println("LEMN Admin CLI")
	fmt.Println("  lemn pending           - List OBSERVED, CANDIDATE, pending, and revalidation memories")
	fmt.Println("  lemn confirm <id> [id...] [--relation independent|supersedes|contradicts] [--target id] [--depends-on id,id|--clear-dependencies]")
	fmt.Println("  lemn revalidate [--evidence-quote passage --evidence-source https_url | --user-confirmed] [--summary revised_summary] [--depends-on id,id | --clear-dependencies] <id>")
	fmt.Println("  lemn reject <id> [id...] - Mark reviewable memories REJECTED")
	fmt.Println("  lemn sweep [--apply]   - Re-apply auto-review rules to the queue (dry run without --apply)")
}

func sweepPending(db *sql.DB, apply bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	actions, err := lemn.SweepPending(ctx, db, apply)
	failed := err != nil
	for _, a := range actions {
		summary := a.Summary
		if len(summary) > 70 {
			summary = summary[:70] + "..."
		}
		fmt.Printf("#%-4d %-12s %s\n      %s\n", a.ID, a.Action, a.Reason, summary)
		if a.Err != nil {
			fmt.Fprintf(os.Stderr, "      error: %v\n", a.Err)
			failed = true
		}
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "sweep stopped: %v\n", err)
	}
	if !apply {
		// Applying one supersede can invalidate later targets, so --apply may differ from this preview.
		fmt.Println("\nDry run. Re-run with --apply to execute.")
	}
	if failed {
		os.Exit(1)
	}
}

func listPending(db *sql.DB) {
	rows, err := db.Query(`
		SELECT id, state, project_id, category, summary, confidence, provenance 
		FROM lemn_memories 
		WHERE state IN ('OBSERVED', 'CANDIDATE', 'PENDING', 'PENDING_CONFIRMATION', 'NEEDS_REVALIDATION')
		ORDER BY created_at DESC;`)
	if err != nil {
		log.Fatalf("Failed to query pending memories: %v", err)
	}
	defer rows.Close()

	fmt.Println("\n--- PENDING MEMORIES FOR HUMAN REVIEW ---")
	count := 0
	for rows.Next() {
		var id int
		var state, projectID, category, summary string
		var confidence float64
		var provenanceJSON []byte

		rows.Scan(&id, &state, &projectID, &category, &summary, &confidence, &provenanceJSON)
		count++

		fmt.Printf("\n[#%d] State: %s | Project: %s | Category: %s | Confidence: %.2f\n", id, state, projectID, category, confidence)
		fmt.Printf("      Summary: %s\n", summary)
		if len(provenanceJSON) > 0 {
			var prov map[string]interface{}
			json.Unmarshal(provenanceJSON, &prov)
			for _, key := range []string{"proposed_relation", "proposed_target_id", "depends_on", "candidate_targets", "evidence_source", "signals", "revalidation_reason", "invalidated_by_memory_id", "invalidated_dependency_id", "invalidated_at", "revalidation_history"} {
				if value, ok := prov[key]; ok {
					encoded, _ := json.Marshal(value)
					fmt.Printf("      %s: %s\n", key, encoded)
				}
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

func confirmMemories(db *sql.DB, args []string) {
	if len(args) == 0 {
		fmt.Println("Usage: lemn confirm <memory_id> [memory_id...] [--relation independent|supersedes|contradicts] [--target id] [--depends-on id,id|--clear-dependencies]")
		return
	}
	// Split flag args from positional ids so that
	// `lemn confirm 5 --relation x` and `lemn confirm --relation x 5 6`
	// are equivalent (the flag package stops at the first non-flag arg).
	flagTakesValue := map[string]bool{"relation": true, "target": true, "depends-on": true}
	var flagArgs, ids []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if strings.HasPrefix(a, "-") {
			name := strings.TrimLeft(a, "-")
			if eq := strings.IndexByte(name, '='); eq >= 0 {
				name = name[:eq]
			}
			flagArgs = append(flagArgs, a)
			if flagTakesValue[name] && !strings.Contains(a, "=") && i+1 < len(args) {
				i++
				flagArgs = append(flagArgs, args[i])
			}
			continue
		}
		ids = append(ids, a)
	}
	flags := flag.NewFlagSet("confirm", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	relation := flags.String("relation", "", "override the proposed relation")
	targetID := flags.Int("target", 0, "relation target memory id")
	dependsOn := flags.String("depends-on", "", "replace dependencies with comma-separated memory ids")
	clearDependencies := flags.Bool("clear-dependencies", false, "remove all proposed dependencies")
	if err := flags.Parse(flagArgs); err != nil {
		return
	}
	if len(ids) == 0 {
		fmt.Println("Usage: lemn confirm <memory_id> [memory_id...] [--relation independent|supersedes|contradicts] [--target id] [--depends-on id,id|--clear-dependencies]")
		return
	}
	if *dependsOn != "" && *clearDependencies {
		fmt.Fprintln(os.Stderr, "use either --depends-on or --clear-dependencies")
		return
	}
	decision := lemn.ConfirmationDecision{Relation: *relation, TargetID: *targetID}
	if *dependsOn != "" || *clearDependencies {
		decision.ReplaceDependencies = true
		if *dependsOn != "" {
			for _, rawID := range strings.Split(*dependsOn, ",") {
				dependencyID, err := strconv.Atoi(strings.TrimSpace(rawID))
				if err != nil || dependencyID <= 0 {
					fmt.Fprintf(os.Stderr, "invalid dependency id %q\n", rawID)
					return
				}
				decision.DependsOn = append(decision.DependsOn, dependencyID)
			}
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	failed := false
	for _, idStr := range ids {
		id, err := strconv.Atoi(idStr)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Invalid memory id %q: %v\n", idStr, err)
			failed = true
			continue
		}
		if err := lemn.ConfirmPendingMemory(ctx, db, id, decision); err != nil {
			fmt.Fprintf(os.Stderr, "Failed to confirm memory #%d: %v\n", id, err)
			failed = true
			continue
		}
		fmt.Printf("Memory #%d confirmed as AUTHORITATIVE; proposed relation and dependencies were applied.\n", id)
	}
	if failed {
		os.Exit(1)
	}
}

func revalidateMemory(db *sql.DB, args []string) {
	flagTakesValue := map[string]bool{"evidence-quote": true, "evidence-source": true, "summary": true, "depends-on": true}
	var flagArgs, ids []string
	for index := 0; index < len(args); index++ {
		arg := args[index]
		if !strings.HasPrefix(arg, "-") {
			ids = append(ids, arg)
			continue
		}
		name := strings.TrimLeft(arg, "-")
		if equals := strings.IndexByte(name, '='); equals >= 0 {
			name = name[:equals]
		}
		flagArgs = append(flagArgs, arg)
		if flagTakesValue[name] && !strings.Contains(arg, "=") && index+1 < len(args) {
			index++
			flagArgs = append(flagArgs, args[index])
		}
	}

	flags := flag.NewFlagSet("revalidate", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	evidence := flags.String("evidence-quote", "", "exact passage expected to appear in the fetched source")
	evidenceSource := flags.String("evidence-source", "", "HTTPS source URL on an approved host")
	userConfirmed := flags.Bool("user-confirmed", false, "record explicit user confirmation")
	summary := flags.String("summary", "", "replace the memory summary")
	dependsOn := flags.String("depends-on", "", "replace dependencies with comma-separated memory IDs")
	clearDependencies := flags.Bool("clear-dependencies", false, "remove all dependencies")
	if err := flags.Parse(flagArgs); err != nil {
		return
	}
	if len(ids) != 1 {
		fmt.Fprintln(os.Stderr, "Usage: lemn revalidate [--evidence-quote passage --evidence-source https_url | --user-confirmed] [--summary revised_summary] [--depends-on id,id | --clear-dependencies] <memory_id>")
		return
	}
	if *dependsOn != "" && *clearDependencies {
		fmt.Fprintln(os.Stderr, "use either --depends-on or --clear-dependencies")
		return
	}
	memoryID, err := strconv.Atoi(ids[0])
	if err != nil || memoryID <= 0 {
		fmt.Fprintf(os.Stderr, "invalid memory ID %q\n", ids[0])
		return
	}
	decision := lemn.RevalidationDecision{
		Evidence:       *evidence,
		EvidenceSource: *evidenceSource,
		UserConfirmed:  *userConfirmed,
		Summary:        *summary,
	}
	if *dependsOn != "" || *clearDependencies {
		decision.ReplaceDependencies = true
		if *dependsOn != "" {
			for _, rawID := range strings.Split(*dependsOn, ",") {
				dependencyID, err := strconv.Atoi(strings.TrimSpace(rawID))
				if err != nil || dependencyID <= 0 {
					fmt.Fprintf(os.Stderr, "invalid dependency ID %q\n", rawID)
					return
				}
				decision.DependsOn = append(decision.DependsOn, dependencyID)
			}
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := lemn.RevalidateMemory(ctx, db, memoryID, decision); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to revalidate memory #%d: %v\n", memoryID, err)
		os.Exit(1)
	}
	fmt.Printf("Memory #%d revalidated as AUTHORITATIVE.\n", memoryID)
}

func rejectMemories(db *sql.DB, idStrs []string) {
	failed := false
	for _, idStr := range idStrs {
		result, err := db.Exec(`UPDATE lemn_memories SET state = 'REJECTED'
			WHERE id = $1 AND state IN ('OBSERVED', 'CANDIDATE', 'PENDING', 'PENDING_CONFIRMATION', 'NEEDS_REVALIDATION')`, idStr)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Memory #%s: failed to reject: %v\n", idStr, err)
			failed = true
			continue
		}
		if n, _ := result.RowsAffected(); n == 0 {
			fmt.Printf("Memory #%s was not found in a reviewable state.\n", idStr)
			continue
		}
		fmt.Printf("Memory #%s marked as REJECTED.\n", idStr)
	}
	if failed {
		os.Exit(1)
	}
}
