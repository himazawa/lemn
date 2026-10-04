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
		if err := listPending(db); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
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
		if err := rejectMemories(db, os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
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
	fmt.Println("  lemn reject <id> [id...] [--reason text] - Reject reviewable memories or retract authoritative memories (reason required)")
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

func listPending(db *sql.DB) error {
	rows, err := db.Query(`
		SELECT memory.id, memory.state, memory.project_id, memory.category,
		       memory.summary, memory.confidence, memory.provenance, memory.created_at,
		       COALESCE((
		         SELECT jsonb_agg(jsonb_build_object(
		           'id', refs.id, 'source', refs.source,
		           'state', dependency.state, 'project_id', dependency.project_id)
		           ORDER BY refs.source, refs.id::text)
		         FROM (
		           SELECT to_jsonb(edge.target_id) AS id, 'edge' AS source
		           FROM lemn_edges edge
		           WHERE edge.source_id = memory.id AND edge.relationship = 'depends_on'
		           UNION ALL
		           SELECT proposed.value AS id, 'provenance' AS source
		           FROM jsonb_array_elements(CASE
		             WHEN jsonb_typeof(memory.provenance->'depends_on') = 'array'
		               THEN memory.provenance->'depends_on'
		             WHEN memory.provenance->'depends_on' IS NULL
		               OR memory.provenance->'depends_on' = 'null'::jsonb THEN '[]'::jsonb
		             ELSE jsonb_build_array(memory.provenance->'depends_on')
		           END) AS proposed(value)
		         ) refs
		         LEFT JOIN lemn_memories dependency ON dependency.id::text = refs.id #>> '{}'
		       ), '[]'::jsonb)
		FROM lemn_memories memory
		WHERE memory.state IN ('OBSERVED', 'CANDIDATE', 'PENDING', 'PENDING_CONFIRMATION', 'NEEDS_REVALIDATION')
		ORDER BY memory.created_at DESC;`)
	if err != nil {
		return fmt.Errorf("failed to query pending memories: %w", err)
	}
	defer rows.Close()

	fmt.Println("\n--- PENDING MEMORIES FOR HUMAN REVIEW ---")
	count := 0
	now := time.Now()
	for rows.Next() {
		var id int
		var state, projectID, category, summary string
		var confidence float64
		var provenanceJSON, dependenciesJSON []byte
		var createdAt time.Time

		if err := rows.Scan(&id, &state, &projectID, &category, &summary, &confidence, &provenanceJSON, &createdAt, &dependenciesJSON); err != nil {
			return fmt.Errorf("failed to scan pending memory: %w", err)
		}
		var prov map[string]interface{}
		if err := json.Unmarshal(provenanceJSON, &prov); err != nil {
			return fmt.Errorf("failed to parse provenance for #%d: %w", id, err)
		}
		targetID := 0
		if relation, _ := prov["proposed_relation"].(string); relation == "supersedes" || relation == "contradicts" {
			if rawTarget, ok := prov["proposed_target_id"]; ok {
				encoded, err := json.Marshal(rawTarget)
				if err != nil {
					return fmt.Errorf("failed to encode relation target for #%d: %w", id, err)
				}
				targetID, _ = reviewDependencyID(encoded)
			}
		}
		dependencies, err := formatReviewDependencies(dependenciesJSON, id, targetID, projectID)
		if err != nil {
			return fmt.Errorf("failed to parse dependencies for #%d: %w", id, err)
		}
		count++

		fmt.Printf("\n[#%d] State: %s | Project: %s | Category: %s | Confidence: %.2f\n", id, state, projectID, category, confidence)
		fmt.Printf("      Created: %s | Age: %s\n", createdAt.Format(time.RFC3339), reviewAge(createdAt, now))
		fmt.Printf("      Summary: %s\n", summary)
		if len(prov) > 0 {
			for _, key := range []string{"proposed_relation", "proposed_target_id", "depends_on", "candidate_targets", "duplicate_candidate_ids", "model_equivalent", "duplicate_review_required", "review_reason", "review_actor", "reviewed_at", "evidence_source", "signals", "revalidation_reason", "invalidated_by_memory_id", "invalidated_dependency_id", "invalidated_at", "revalidation_history"} {
				if value, ok := prov[key]; ok {
					encoded, err := json.Marshal(value)
					if err != nil {
						return fmt.Errorf("failed to encode %s for #%d: %w", key, id, err)
					}
					fmt.Printf("      %s: %s\n", key, encoded)
				}
			}
		}
		for _, dependency := range dependencies {
			fmt.Printf("      %s\n", dependency)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("failed while iterating pending memories: %w", err)
	}

	if count == 0 {
		fmt.Println("No pending memories requiring review.")
	}
	return nil
}

func reviewAge(createdAt, now time.Time) string {
	age := now.Sub(createdAt)
	if age < 0 {
		age = 0
	}
	label := fmt.Sprintf("%dd %dh", int(age.Hours())/24, int(age.Hours())%24)
	if age > 7*24*time.Hour {
		label += " | NEEDS ATTENTION (>7 days)"
	}
	return label
}

func reviewDependencyID(raw json.RawMessage) (int, error) {
	text := string(raw)
	if strings.HasPrefix(text, `"`) {
		if err := json.Unmarshal(raw, &text); err != nil {
			return 0, err
		}
	}
	id, err := strconv.Atoi(text)
	if err != nil {
		return 0, err
	}
	if id <= 0 {
		return 0, fmt.Errorf("dependency ID must be positive")
	}
	return id, nil
}

func formatReviewDependencies(raw []byte, memoryID, targetID int, projectID string) ([]string, error) {
	var dependencies []struct {
		ID        json.RawMessage `json:"id"`
		Source    string          `json:"source"`
		State     *string         `json:"state"`
		ProjectID *string         `json:"project_id"`
	}
	if err := json.Unmarshal(raw, &dependencies); err != nil {
		return nil, err
	}
	var lines []string
	for _, dependency := range dependencies {
		id, err := reviewDependencyID(dependency.ID)
		var reasons []string
		if err != nil {
			reasons = append(reasons, "invalid dependency ID")
		} else if id == memoryID || id == targetID {
			reasons = append(reasons, "invalid dependency ID (self or relation target)")
		}
		state, scope := "missing", "missing"
		if dependency.State == nil || dependency.ProjectID == nil {
			if err == nil {
				reasons = append(reasons, "dependency ID not found")
			}
		} else {
			state, scope = *dependency.State, *dependency.ProjectID
			if state != "AUTHORITATIVE" {
				reasons = append(reasons, "invalid state: "+state)
			}
			if scope != projectID && !(projectID != "global" && scope == "global") {
				reasons = append(reasons, "invalid scope: "+scope)
			}
		}
		label := fmt.Sprintf("depends_on (%s): %s | State: %s | Project: %s", dependency.Source, dependency.ID, state, scope)
		if len(reasons) > 0 {
			label += " | BLOCKED: " + strings.Join(reasons, "; ")
		}
		lines = append(lines, label)
	}
	return lines, nil
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

func rejectMemories(db *sql.DB, args []string) error {
	ids, reason, err := parseRejectArgs(args)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	failed := 0
	for _, id := range ids {
		if err := lemn.RejectMemory(ctx, db, id, reason); err != nil {
			fmt.Fprintf(os.Stderr, "Memory #%d: failed to reject: %v\n", id, err)
			failed++
			continue
		}
		fmt.Printf("Memory #%d marked as REJECTED.\n", id)
	}
	if failed > 0 {
		return fmt.Errorf("failed to reject %d memories", failed)
	}
	return nil
}

func parseRejectArgs(args []string) ([]int, string, error) {
	var flagArgs, rawIDs []string
	for index := 0; index < len(args); index++ {
		arg := args[index]
		if !strings.HasPrefix(arg, "-") {
			rawIDs = append(rawIDs, arg)
			continue
		}
		flagArgs = append(flagArgs, arg)
		if (arg == "--reason" || arg == "-reason") && index+1 < len(args) {
			index++
			flagArgs = append(flagArgs, args[index])
		}
	}
	flags := flag.NewFlagSet("reject", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	reason := flags.String("reason", "", "admin rejection or retraction reason")
	if err := flags.Parse(flagArgs); err != nil {
		return nil, "", err
	}
	if len(rawIDs) == 0 || flags.NArg() != 0 {
		return nil, "", fmt.Errorf("Usage: lemn reject <memory_id> [memory_id...] [--reason text]")
	}
	ids := make([]int, 0, len(rawIDs))
	for _, rawID := range rawIDs {
		id, err := strconv.Atoi(rawID)
		if err != nil || id <= 0 {
			return nil, "", fmt.Errorf("invalid memory id %q", rawID)
		}
		ids = append(ids, id)
	}
	return ids, strings.TrimSpace(*reason), nil
}
