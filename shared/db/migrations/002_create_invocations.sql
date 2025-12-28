CREATE TABLE IF NOT EXISTS invocations (
    id TEXT PRIMARY KEY,
    function_id TEXT NOT NULL,
    status TEXT NOT NULL,
    duration_ms INTEGER,
    started_at DATETIME NOT NULL,
    error_message TEXT,
    FOREIGN KEY (function_id) REFERENCES functions(id) ON DELETE CASCADE
);

CREATE INDEX idx_invocations_function_id ON invocations(function_id);
CREATE INDEX idx_invocations_started_at ON invocations(started_at);
