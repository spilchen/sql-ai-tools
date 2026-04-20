-- EXPLAIN variants: basic, ANALYZE, options (DDL, DISTSQL, OPT, VERBOSE),
-- and EXPLAIN applied to DDL statements.
--
-- Source: pkg/sql/parser/testdata/explain (cockroachdb/cockroach@release-25.3)

EXPLAIN SELECT * FROM orders WHERE id = 1;

EXPLAIN (VERBOSE) SELECT * FROM orders JOIN customers ON orders.cid = customers.id;

EXPLAIN (OPT) SELECT * FROM orders WHERE status = 'pending';

EXPLAIN (OPT, VERBOSE) SELECT a, sum(b) FROM t GROUP BY a;

EXPLAIN (DISTSQL) SELECT * FROM orders WHERE region = 'us-east1';

EXPLAIN (DISTSQL, JSON) SELECT * FROM large_table;

EXPLAIN ANALYZE SELECT * FROM orders WHERE total > 1000;

EXPLAIN ANALYZE (DISTSQL) SELECT * FROM orders AS o JOIN items AS i ON o.id = i.order_id;

EXPLAIN (DDL) CREATE TABLE new_tbl (id INT PRIMARY KEY, val STRING);

EXPLAIN CREATE TABLE new_tbl (id INT PRIMARY KEY, val STRING);

EXPLAIN ALTER TABLE orders ADD COLUMN notes STRING;
