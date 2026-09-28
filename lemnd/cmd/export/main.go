package main

import (
	"encoding/json"
	"fmt"
	"os"

	"lemnd/internal/logstore"
)

type ChatMLItem struct {
	Messages []map[string]string `json:"messages"`
}

func main() {
	db, err := logstore.Open()
	if err != nil {
		fmt.Println("Failed to open DB:", err)
		return
	}
	defer db.Close()

	rows, err := db.Query("SELECT user_message, assistant_response, is_memory_worthy, memory_type, extracted_summary, confidence FROM turns WHERE human_reviewed = TRUE")
	if err != nil {
		fmt.Println("Failed to query reviewed turns:", err)
		return
	}
	defer rows.Close()

	if err := os.MkdirAll("./data", 0755); err != nil {
		fmt.Println("Failed to create data directory:", err)
		return
	}
	trainFile, err := os.Create("./data/train.jsonl")
	if err != nil {
		fmt.Println("Failed to create train file:", err)
		return
	}
	defer trainFile.Close()
	validFile, err := os.Create("./data/valid.jsonl")
	if err != nil {
		fmt.Println("Failed to create valid file:", err)
		return
	}
	defer validFile.Close()

	index := 0
	for rows.Next() {
		var userMsg, asstMsg, memType, summary string
		var worthy bool
		var conf float64
		if err := rows.Scan(&userMsg, &asstMsg, &worthy, &memType, &summary, &conf); err != nil {
			fmt.Println("Skipping row with scan error:", err)
			continue
		}

		userPrompt := fmt.Sprintf("Analyze this turn:\nUser: %s\nAssistant: %s", userMsg, asstMsg)
		assistantContent, err := json.Marshal(map[string]any{
			"memory_worthy": worthy,
			"type":          memType,
			"summary":       summary,
			"confidence":    conf,
		})
		if err != nil {
			fmt.Println("Skipping row with marshal error:", err)
			continue
		}

		item := ChatMLItem{
			Messages: []map[string]string{
				{"role": "user", "content": userPrompt},
				{"role": "assistant", "content": string(assistantContent)},
			},
		}

		line, err := json.Marshal(item)
		if err != nil {
			fmt.Println("Skipping row with json error:", err)
			continue
		}
		line = append(line, '\n')

		if index%5 == 0 {
			if _, err := validFile.Write(line); err != nil {
				fmt.Println("Failed to write valid row:", err)
				return
			}
		} else {
			if _, err := trainFile.Write(line); err != nil {
				fmt.Println("Failed to write train row:", err)
				return
			}
		}
		index++
	}
	if err := rows.Err(); err != nil {
		fmt.Println("Failed while iterating reviewed turns:", err)
		return
	}
	fmt.Printf("Exported %d human-reviewed items to ./data/train.jsonl and ./data/valid.jsonl\n", index)
}
