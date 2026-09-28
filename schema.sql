CREATE EXTENSION IF NOT EXISTS vector;

CREATE TABLE lemn_memories (
    id SERIAL PRIMARY KEY,
    created_at TIMESTAMP WITH TIME ZONE DEFAULT CURRENT_TIMESTAMP,
    state TEXT NOT NULL DEFAULT 'CANDIDATE', -- OBSERVED, CANDIDATE, AUTHORITATIVE, PENDING, PENDING_CONFIRMATION, SUPERSEDED, CONTRADICTED, NEEDS_REVALIDATION, REJECTED
    confidence REAL NOT NULL,
    category TEXT NOT NULL,
    summary TEXT NOT NULL,
    rationale TEXT,
    embedding vector(1536),
    provenance JSONB NOT NULL
);

CREATE TABLE lemn_edges (
    source_id INT REFERENCES lemn_memories(id),
    target_id INT REFERENCES lemn_memories(id),
    relationship TEXT NOT NULL, -- "supersedes", "contradicts", "depends_on"
    PRIMARY KEY (source_id, target_id)
);
