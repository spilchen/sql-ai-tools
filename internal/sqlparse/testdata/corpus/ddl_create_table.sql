-- CREATE TABLE variants: plain, computed columns, FAMILY clauses,
-- hash-sharded primary keys and unique indexes.
--
-- Source: pkg/sql/parser/testdata/create_table (cockroachdb/cockroach@release-25.3)

CREATE TABLE basic (id INT8 PRIMARY KEY, name STRING NOT NULL, active BOOL DEFAULT true);

CREATE TABLE with_computed (
  a INT8,
  b INT8,
  sum INT8 AS (a + b) STORED,
  label STRING AS (concat('row_', a::STRING)) VIRTUAL
);

CREATE TABLE with_families (
  id INT8 PRIMARY KEY,
  name STRING,
  payload BYTES,
  FAMILY fam_core (id, name),
  FAMILY fam_blob (payload)
);

CREATE TABLE with_hash_unique (
  id INT PRIMARY KEY,
  tenant_id INT,
  UNIQUE INDEX (tenant_id) USING HASH WITH BUCKET_COUNT = 8
);

CREATE TABLE with_pk_hash (
  region STRING,
  id UUID,
  data JSONB,
  PRIMARY KEY (region, id) USING HASH WITH BUCKET_COUNT = 16
);

CREATE TABLE full_featured (
  id INT8 NOT NULL,
  ts TIMESTAMPTZ DEFAULT now(),
  body STRING,
  search_tsv TSVECTOR AS (to_tsvector('english', body)) STORED,
  CONSTRAINT pk PRIMARY KEY (id),
  FAMILY fam_meta (id, ts),
  FAMILY fam_content (body, search_tsv)
);
