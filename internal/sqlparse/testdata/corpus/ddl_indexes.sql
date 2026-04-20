-- CREATE INDEX variants: basic, partial (WHERE), inverted/GIN, STORING.
--
-- Source: pkg/sql/parser/testdata/create_index (cockroachdb/cockroach@release-25.3)

CREATE INDEX idx_name ON orders (customer_id);

CREATE INDEX ON orders (created_at DESC);

CREATE UNIQUE INDEX idx_email ON users (email);

CREATE INDEX ON orders (status) STORING (total, customer_id);

CREATE INDEX ON orders (region, created_at) WHERE status = 'active';

CREATE UNIQUE INDEX ON users (org_id, email) WHERE deleted_at IS NULL;

CREATE INVERTED INDEX ON documents (metadata);

CREATE INVERTED INDEX idx_tags ON items (tags) STORING (name);

CREATE INVERTED INDEX ON events (payload) WHERE kind = 'audit';

CREATE INDEX idx_gin ON items USING GIN (tags);
