-- DML mutation variants: INSERT ON CONFLICT, UPSERT, UPDATE FROM,
-- DELETE USING, RETURNING.
--
-- Source: pkg/sql/parser/testdata/upsert (cockroachdb/cockroach@release-25.3)
-- Source: pkg/sql/parser/testdata/delete (cockroachdb/cockroach@release-25.3)

INSERT INTO kv (k, v) VALUES ('a', 1) ON CONFLICT (k) DO NOTHING;

INSERT INTO kv (k, v) VALUES ('a', 1)
  ON CONFLICT (k) DO UPDATE SET v = excluded.v
  WHERE kv.v < excluded.v;

INSERT INTO kv (k, v) VALUES ('a', 1), ('b', 2)
  ON CONFLICT (k) DO UPDATE SET v = excluded.v
  RETURNING k, v;

UPSERT INTO kv (k, v) VALUES ('x', 10);

UPSERT INTO kv SELECT k, v FROM staging;

UPDATE accounts SET balance = accounts.balance + t.amount
  FROM transactions AS t
  WHERE accounts.id = t.account_id AND t.applied = false
  RETURNING accounts.id, accounts.balance AS new_balance;

UPDATE orders SET status = 'shipped', shipped_at = now()
  FROM shipments AS s
  WHERE orders.id = s.order_id AND s.carrier_confirmed = true;

DELETE FROM sessions USING users
  WHERE sessions.user_id = users.id AND users.disabled = true
  RETURNING sessions.id;

DELETE FROM expired_tokens WHERE expires_at < now() RETURNING *;
