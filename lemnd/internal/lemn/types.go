package lemn

type ToolCallEvidence struct {
	Name     string `json:"name"`
	DiffText string `json:"diff_text"` // git diff, file edit, or tool output
}

type TurnPayload struct {
	ID                string             `json:"id"`
	UserMessage       string             `json:"user_message"`
	AssistantResponse string             `json:"assistant_response"`
	ToolCalls         []ToolCallEvidence `json:"tool_calls"`
}

type ModelExtraction struct {
	MemoryWorthy bool    `json:"memory_worthy"`
	Type         string  `json:"type"` // "decision", "architecture", "bug_fix", "none"
	Summary      string  `json:"summary"`
	Confidence   float64 `json:"confidence"` // 0.0 - 1.0
}

type MemoryNode struct {
	ID         int                    `json:"id"`
	State      string                 `json:"state"`
	Confidence float64                `json:"confidence"`
	Category   string                 `json:"category"`
	Summary    string                 `json:"summary"`
	Rationale  string                 `json:"rationale"`
	Embedding  []float32              `json:"embedding"`
	Provenance map[string]interface{} `json:"provenance"`
}

type MatchTarget struct {
	ID         int     `json:"id"`
	Summary    string  `json:"summary"`
	Similarity float64 `json:"similarity"`
}

type CorrectionIntent struct {
	IsExplicitOverride bool
	UserMessage        string
}

type RetrievalRequest struct {
	Query string `json:"query"`
	Limit int    `json:"limit"`
}

type RetrievedMemory struct {
	ID         int     `json:"id"`
	Category   string  `json:"category"`
	Summary    string  `json:"summary"`
	Similarity float64 `json:"similarity"`
}
