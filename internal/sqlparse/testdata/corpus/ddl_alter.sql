-- ALTER TABLE / ALTER PRIMARY KEY / CONFIGURE ZONE variants.
--
-- Source: pkg/sql/parser/testdata/alter_table (cockroachdb/cockroach@release-25.3)

ALTER TABLE users ADD COLUMN email STRING NOT NULL;

ALTER TABLE users ADD COLUMN IF NOT EXISTS phone STRING;

ALTER TABLE users ADD COLUMN bio STRING CREATE FAMILY fam_bio;

ALTER TABLE orders ALTER PRIMARY KEY USING COLUMNS (region, id);

ALTER TABLE events ADD CONSTRAINT events_user_fk FOREIGN KEY (user_id) REFERENCES users (id);

ALTER TABLE users ADD COLUMN role STRING DEFAULT 'reader', ADD CONSTRAINT users_role_check CHECK (role IN ('reader', 'writer', 'admin'));

ALTER TABLE regional_orders CONFIGURE ZONE USING
  num_replicas = 5,
  constraints = '[+region=us-east1]',
  lease_preferences = '[[+region=us-east1]]';

ALTER PARTITION us_east OF TABLE orders CONFIGURE ZONE USING
  constraints = '[+region=us-east1]',
  lease_preferences = '[[+region=us-east1]]';

ALTER TABLE ttl_data CONFIGURE ZONE USING gc.ttlseconds = 3600;
