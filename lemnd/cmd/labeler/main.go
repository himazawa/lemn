package main

import (
	"fmt"
	"log"

	"lemnd/internal/logstore"
)

func main() {
	db, err := logstore.Open()
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	rows, err := db.Query("SELECT id, user_message, assistant_response, is_memory_worthy, memory_type, confidence FROM turns WHERE human_reviewed = FALSE LIMIT 300")
	if err != nil {
		log.Fatal(err)
	}
	defer rows.Close()

	for rows.Next() {
		var id, userMsg, asstMsg, memType string
		var worthy bool
		var conf float64
		if err := rows.Scan(&id, &userMsg, &asstMsg, &worthy, &memType, &conf); err != nil {
			log.Printf("Skipping row with scan error: %v", err)
			continue
		}

		fmt.Println("\n==================================================")
		fmt.Printf("ID: %s\n", id)
		fmt.Printf("USER: %s\n", userMsg)
		fmt.Printf("ASST: %s\n", asstMsg)
		fmt.Printf("Model Guessed -> Worthy: %v | Type: %s | Conf: %.2f\n", worthy, memType, conf)
		fmt.Print("Is Memory Worthy? [1=Yes, 0=No, s=Skip]: ")

		var input string
		fmt.Scanln(&input)
		if input == "s" {
			continue
		}

		newWorthy := input == "1"
		fmt.Print("Type [d=decision, a=architecture, b=bug_fix, n=none]: ")
		fmt.Scanln(&input)

		typeMap := map[string]string{"d": "decision", "a": "architecture", "b": "bug_fix", "n": "none"}
		newType := typeMap[input]
		if newType == "" {
			newType = "none"
		}

		if _, err := db.Exec("UPDATE turns SET is_memory_worthy = ?, memory_type = ?, human_reviewed = TRUE WHERE id = ?", newWorthy, newType, id); err != nil {
			log.Printf("Failed to update record: %v", err)
		}
	}
	if err := rows.Err(); err != nil {
		log.Fatalf("Failed while iterating unreviewed turns: %v", err)
	}
	fmt.Println("Labeling session finished!")
}
