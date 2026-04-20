-- Multi-region: ALTER DATABASE region management, ALTER TABLE SET LOCALITY,
-- SURVIVE REGION FAILURE.
--
-- Source: pkg/sql/parser/testdata/alter_database (cockroachdb/cockroach@release-25.3)
-- Source: pkg/sql/parser/testdata/alter_table (cockroachdb/cockroach@release-25.3)
-- Source: pkg/sql/parser/testdata/create_table (cockroachdb/cockroach@release-25.3)

ALTER DATABASE mydb PRIMARY REGION "us-east1";

ALTER DATABASE mydb ADD REGION "us-west1";

ALTER DATABASE mydb ADD REGION IF NOT EXISTS "eu-west1";

ALTER DATABASE mydb DROP REGION "eu-west1";

ALTER DATABASE mydb SURVIVE REGION FAILURE;

CREATE TABLE global_config (key STRING PRIMARY KEY, val STRING) LOCALITY GLOBAL;

CREATE TABLE regional_orders (
  id UUID PRIMARY KEY,
  region STRING NOT NULL,
  total DECIMAL
) LOCALITY REGIONAL BY TABLE IN "us-east1";

CREATE TABLE row_regional (
  id UUID PRIMARY KEY,
  crdb_region STRING NOT NULL,
  data JSONB
) LOCALITY REGIONAL BY ROW;

CREATE TABLE row_regional_as (
  id UUID PRIMARY KEY,
  home_region STRING NOT NULL,
  data JSONB
) LOCALITY REGIONAL BY ROW AS home_region;

ALTER TABLE orders SET LOCALITY REGIONAL BY ROW;

ALTER TABLE config SET LOCALITY GLOBAL;
