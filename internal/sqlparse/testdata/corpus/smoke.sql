-- Placeholder corpus entry for the fidelity test suite.
--
-- This file exists so the directory walk in fidelity_test.go has at
-- least one canonical statement to parse end-to-end. Real coverage
-- (DDL / DML / EXPLAIN / multi-region) is tracked under a separate
-- follow-up issue; do not grow this file in place — add new files
-- per topic instead.
--
-- A multi-statement body also confirms parser.Parse handles ';'
-- separation, which the rest of the corpus will rely on.

CREATE TABLE smoke (id INT PRIMARY KEY, name STRING NOT NULL);
SELECT id, name FROM smoke WHERE id > 0;
