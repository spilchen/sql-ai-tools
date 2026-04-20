-- SELECT variants: joins, CTEs, window functions, FOR UPDATE.
--
-- Source: pkg/sql/parser/testdata/select_clauses (cockroachdb/cockroach@release-25.3)
-- Source: pkg/sql/parser/testdata/common_table_exprs (cockroachdb/cockroach@release-25.3)

SELECT o.id, c.name
  FROM orders AS o
  JOIN customers AS c ON o.customer_id = c.id
 WHERE o.total > 100;

SELECT a.id, b.val
  FROM t1 AS a
  INNER MERGE JOIN t2 AS b ON a.id = b.id;

SELECT u.name, p.title
  FROM users AS u
  LEFT JOIN posts AS p ON u.id = p.author_id;

SELECT *
  FROM left_tbl AS a
  FULL OUTER JOIN right_tbl AS b ON a.key = b.key
 ORDER BY a.key, b.key;

WITH recent AS (
  SELECT id, name FROM users WHERE created_at > now() - INTERVAL '7 days'
)
SELECT r.id, r.name, count(o.id) AS order_count
  FROM recent AS r
  JOIN orders AS o ON r.id = o.user_id
 GROUP BY r.id, r.name;

WITH RECURSIVE subordinates (id, manager_id, depth) AS (
  SELECT id, manager_id, 0 FROM employees WHERE manager_id IS NULL
  UNION ALL
  SELECT e.id, e.manager_id, s.depth + 1
    FROM employees AS e
    JOIN subordinates AS s ON e.manager_id = s.id
)
SELECT * FROM subordinates;

WITH snapshot AS MATERIALIZED (
  SELECT id, balance FROM accounts
)
SELECT id, balance FROM snapshot WHERE balance < 0;

SELECT id, amount,
       sum(amount) OVER (PARTITION BY customer_id ORDER BY created_at) AS running_total,
       row_number() OVER (PARTITION BY customer_id ORDER BY created_at DESC) AS rn
  FROM payments;

SELECT 1 FOR UPDATE;

SELECT id, stock FROM inventory WHERE warehouse = 'east' FOR UPDATE SKIP LOCKED;

SELECT id FROM jobs WHERE status = 'pending' FOR UPDATE NOWAIT;

SELECT a.id FROM t1 AS a FOR UPDATE OF a FOR SHARE OF t2;
