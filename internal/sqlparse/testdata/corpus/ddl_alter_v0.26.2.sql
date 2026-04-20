-- minparser: v0.26.2
--
-- ALTER TABLE ENABLE/DISABLE TRIGGER and ALTER INDEX SET/RESET,
-- added in release-26.2.
--
-- Source: pkg/sql/parser/testdata/alter_table diff release-26.1..release-26.2
--         (cockroachdb/cockroach)
-- Source: pkg/sql/parser/testdata/alter_index diff release-26.1..release-26.2
--         (cockroachdb/cockroach)

ALTER TABLE audit_log ENABLE TRIGGER notify_insert;

ALTER TABLE audit_log DISABLE TRIGGER notify_insert;

ALTER TABLE audit_log ENABLE TRIGGER ALL;

ALTER TABLE audit_log DISABLE TRIGGER ALL;

ALTER INDEX orders@idx_status SET (fillfactor = 80);

ALTER INDEX orders@idx_status RESET (fillfactor);
