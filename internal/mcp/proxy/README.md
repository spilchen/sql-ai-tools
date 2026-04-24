# proxy

Per-call `target_version` routing for the long-lived `crdb-sql mcp`
server. When a tool call's resolved `target_version` quarter does not
match the running binary's quarter, `internal/mcp/routing.go` hands
off to a `Router` here, which forwards the call to the matching
sibling `crdb-sql-vXXX` backend and returns the sibling's
`*mcp.CallToolResult` verbatim. The visible signal that routing
fired is the sibling's `parser_version` stamp on the envelope.

## Routers

- **`Router`** — interface. `Dispatch(ctx, want, req)` returns the
  sibling's result. Transport failures (spawn, init, broken pipe)
  propagate as Go errors; missing-sibling and tool-level failures
  come back as `IsError=true` `*mcp.CallToolResult` with a nil
  error. See the doc on the interface for the full contract.

- **`NoopRouter`** — default when routing is not wired into a server
  (e.g. unit tests of the wrapper that only exercise the
  local-handler path). Every Dispatch returns a tool-error result
  naming the requested sibling, so a wiring bug is loud rather than
  silent.

- **`PoolRouter`** — production implementation. Keeps at most one
  warm sibling child per `versionroute.Quarter` for the life of the
  parent `crdb-sql mcp` process. Lazy spawn on first request, idle
  eviction after 5 minutes (`WithIdleTimeout` to override), and
  transparent re-spawn on transport failure or eviction. Per-quarter
  requests serialize through the entry's mutex because
  `mark3labs/mcp-go`'s stdio `Client` is not safe for concurrent
  `CallTool`. `cmd/mcp.go` defers `Close` after `server.ServeStdio`
  returns so a clean parent exit closes every warm sibling.

## Benchmark

`proxy_bench_test.go` (build tag `integration`) measures one routed
`parse_sql` call under three configurations. Numbers are
developer-tool reference points — there is no CI gate, because
small-machine runners are too noisy.

Run:

```
go test -tags integration -run '^$' \
    -bench=BenchmarkRoute -benchtime=5x ./internal/mcp/proxy
```

### Reference numbers

Single sample, AMD Ryzen 9 5900X (24 logical cores), Linux,
`-benchtime=5x`. Order-of-magnitude only; expect 2–3× variance on
slower hardware or shared CI runners.

| Benchmark               | ns/op       | B/op    | allocs/op | Notes                                                            |
| ----------------------- | ----------- | ------- | --------- | ---------------------------------------------------------------- |
| `RouteSpawnPerCall`     | ~12.5 ms    | ~34 KB  | ~200      | spawn + MCP initialize handshake per call (#129 router, since replaced) |
| `RoutePooledWarm`       | ~0.49 ms    | ~4.7 KB | ~68       | JSON-RPC round-trip only — pool's steady-state cost              |
| `RoutePooledCold`       | ~12.8 ms    | ~35 KB  | ~207      | fresh pool per iteration; matches SpawnPerCall as expected       |

### What this tells you

- Warm reuse is roughly **25× cheaper** per call than spawning. The
  pool earns its complexity any time a quarter receives more than
  one call in its idle window.
- `PooledCold ≈ SpawnPerCall` confirms the pool's only saving is
  reuse — it is not a faster spawn path. There is no point in
  resurrecting `SpawnRouter` for any workload that hits the same
  quarter twice.
- The 5-minute default idle window (`WithIdleTimeout`) is sized for
  agent sessions that re-issue routed calls within a few minutes;
  shorter windows risk paying spawn cost on every request, longer
  windows leak children that are unlikely to be used again. Re-run
  the benchmark before changing the default if request patterns
  shift.

Out of scope (separate investigations if either dominates): JSON-RPC
framing overhead, parser cold-start cost on the first call into a
fresh child.
