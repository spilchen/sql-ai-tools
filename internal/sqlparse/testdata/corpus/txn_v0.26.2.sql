-- minparser: v0.26.2
--
-- Transaction aliases (BEGIN/COMMIT/ROLLBACK WORK) and AND CHAIN,
-- added in release-26.2.
--
-- Source: pkg/sql/parser/testdata/begin_commit diff release-26.1..release-26.2
--         (cockroachdb/cockroach)

BEGIN WORK;
COMMIT WORK;

BEGIN TRANSACTION;
COMMIT AND CHAIN;

BEGIN TRANSACTION;
COMMIT AND NO CHAIN;

ROLLBACK WORK;

BEGIN TRANSACTION;
ROLLBACK AND CHAIN;
