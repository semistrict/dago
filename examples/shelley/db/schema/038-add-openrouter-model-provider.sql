-- Extend the custom-model provider constraint while preserving existing rows.
CREATE TABLE models_with_openrouter (
    model_id TEXT PRIMARY KEY,
    display_name TEXT NOT NULL,
    provider_type TEXT NOT NULL CHECK (provider_type IN ('anthropic', 'openai', 'openai-responses', 'openrouter-responses', 'gemini')),
    endpoint TEXT NOT NULL,
    api_key TEXT NOT NULL,
    model_name TEXT NOT NULL,
    max_tokens INTEGER NOT NULL DEFAULT 200000,
    tags TEXT NOT NULL DEFAULT '',
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    reasoning_effort TEXT NOT NULL DEFAULT '',
    image_support TEXT NOT NULL DEFAULT 'auto',
    reasoning_support TEXT NOT NULL DEFAULT 'auto',
    reasoning_map TEXT NOT NULL DEFAULT ''
);

INSERT INTO models_with_openrouter (
    model_id, display_name, provider_type, endpoint, api_key, model_name,
    max_tokens, tags, created_at, updated_at, reasoning_effort, image_support,
    reasoning_support, reasoning_map
)
SELECT
    model_id, display_name, provider_type, endpoint, api_key, model_name,
    max_tokens, tags, created_at, updated_at, reasoning_effort, image_support,
    reasoning_support, reasoning_map
FROM models;

DROP TABLE models;
ALTER TABLE models_with_openrouter RENAME TO models;
