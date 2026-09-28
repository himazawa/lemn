CREATE EXTENSION IF NOT EXISTS vector;

CREATE TABLE lemn_memories (
    id SERIAL PRIMARY KEY,
    created_at TIMESTAMP WITH TIME ZONE DEFAULT CURRENT_TIMESTAMP,
    state TEXT NOT NULL DEFAULT 'CANDIDATE', -- OBSERVED, CANDIDATE, AUTHORITATIVE, PENDING, PENDING_CONFIRMATION, SUPERSEDED, CONTRADICTED, NEEDS_REVALIDATION, REJECTED
    project_id TEXT NOT NULL DEFAULT 'global', -- 'global' applies everywhere; any other value scopes the memory to one project
    confidence REAL NOT NULL,
    category TEXT NOT NULL,
    summary TEXT NOT NULL,
    rationale TEXT,
    -- Must match the output width of LEMN_EMBEDDING_MODEL (1024 for bge-m3).
    embedding vector(1024),
    provenance JSONB NOT NULL
);

CREATE TABLE lemn_edges (
    source_id INT REFERENCES lemn_memories(id),
    target_id INT REFERENCES lemn_memories(id),
    relationship TEXT NOT NULL, -- "supersedes", "contradicts", "depends_on"
    PRIMARY KEY (source_id, target_id)
);

CREATE INDEX lemn_memories_scope_idx ON lemn_memories (project_id, state);
