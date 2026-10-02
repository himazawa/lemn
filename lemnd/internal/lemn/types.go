package lemn

type ToolCallEvidence struct {
	Name     string `json:"name"`
	DiffText string `json:"diff_text"` // git diff, file edit, or tool output
}

type TurnPayload struct {
	ID                string             `json:"id"`
	ProjectID         string             `json:"project_id"`
	UserMessage       string             `json:"user_message"`
	AssistantResponse string             `json:"assistant_response"`
	ToolCalls         []ToolCallEvidence `json:"tool_calls"`
}

type ModelExtraction struct {
	MemoryWorthy           bool          `json:"memory_worthy"`
	GatePassed             bool          `json:"gate_passed"`
	GlobalScoped           bool          `json:"global_scoped"` // user-level preference rather than a project fact
	Type                   string        `json:"type"`          // "decision", "architecture", "bug_fix", "none"
	Summary                string        `json:"summary"`
	Confidence             float64       `json:"confidence"`         // 0.0 - 1.0
	GateProbability        float64       `json:"gate_probability"`   // Laya's memory-worthiness probability
	GlobalProbability      float64       `json:"global_probability"` // Laya's user-preference probability
	Relation               string        `json:"relation"`           // "independent", "supersedes", or "contradicts"
	TargetID               int           `json:"target_id"`
	DependsOn              []int         `json:"depends_on"`
	DependencyCandidateIDs []int         `json:"-"`
	RelationCandidates     []MatchTarget `json:"-"`
	SummaryEmbedding       []float32     `json:"-"`
}

type MemoryNode struct {
	State      string                 `json:"state"`
	ProjectID  string                 `json:"project_id"`
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

type DependencyCandidate struct {
	ID        int    `json:"id"`
	ProjectID string `json:"project_id"`
	Summary   string `json:"summary"`
}

type CorrectionIntent struct {
	IsExplicitOverride bool
	UserMessage        string
}

type RetrievalRequest struct {
	Query     string `json:"query"`
	ProjectID string `json:"project_id"`
	Limit     int    `json:"limit"`
}

type RetrievedMemory struct {
	ID         int     `json:"id"`
	ProjectID  string  `json:"project_id"`
	Category   string  `json:"category"`
	Summary    string  `json:"summary"`
	Similarity float64 `json:"similarity"`
	// BelowThreshold is set when the near-miss fallback returned this memory:
	// it is AUTHORITATIVE but did not clear the retrieval similarity bar for
	// this query, so the prompt should present it as weaker context.
	BelowThreshold bool `json:"below_threshold"`
}
