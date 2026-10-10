-- Unique index on node credential hashes. Prevents two approved nodes
-- sharing one node_token (ambiguous credential resolution) and speeds
-- up the token lookup on every agent connect.
CREATE UNIQUE INDEX IF NOT EXISTS nodes_token_hash_key ON nodes (token_hash);
