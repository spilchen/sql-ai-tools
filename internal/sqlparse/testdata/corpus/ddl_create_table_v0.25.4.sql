-- minparser: v0.25.4
--
-- CREATE TABLE with LTREE column type, added in release-25.4.
--
-- Source: pkg/sql/parser/testdata/create_table diff release-25.3..release-25.4
--         (cockroachdb/cockroach)

CREATE TABLE paths (id INT8 PRIMARY KEY, path LTREE NOT NULL);

CREATE TABLE categories (
  id INT8 PRIMARY KEY,
  label STRING NOT NULL,
  hierarchy LTREE
);
