-- minparser: v0.26.2
--
-- CREATE INDEX ... USING HASH with bucket_count and shard_columns,
-- added in release-26.2.
--
-- Source: pkg/sql/parser/testdata/create_index diff release-26.1..release-26.2
--         (cockroachdb/cockroach)

CREATE INDEX idx_hash ON orders (customer_id) USING HASH;

CREATE INDEX idx_hash_buckets ON orders (customer_id) USING HASH WITH (bucket_count = 8);

CREATE INDEX idx_hash_storing ON orders (customer_id) USING HASH STORING (total, status);

CREATE INDEX idx_hash_shards ON events (tenant_id, created_at)
  USING HASH WITH (bucket_count = 16, shard_columns = (tenant_id));
