# LAN and linkd transport optimization

Implemented and verified by Codex on November 19, 2026; streaming-cadence correction verified on November 20, 2026 (Asia/Shanghai).

The goal is to reduce redundant state, event framing, and connection setup while preserving authoritative Core state, event order, authentication, and end-to-end TLS. Lossless means that the original event bytes or complete projected state can be reconstructed; it does not imply a theoretical minimum byte count.

## Findings

Native LAN and linkd connections carry TLS. Compression belongs inside that TLS connection; compressing the relay's ciphertext would add work without useful savings. HTTP/1.1 already supports keep-alive, so native requests did not invariably create a new tunnel. However, linkd's HTTP proxy created and closed a Transport for each request, preventing reuse across sequential requests.

T3 subscriptions previously rebuilt and transmitted a full projection on each dirty 50ms tick. Idle ticks did not send snapshots. Gorilla WebSocket 1.5.3 supports only no-context-takeover, so enabling its compression option does not provide a dictionary shared across messages.

## Design and tradeoffs

| Layer | Implementation | Tradeoff or compatibility boundary |
| --- | --- | --- |
| T3 WebSocket | Negotiate permessage-deflate with fast compression; skip outbound frames smaller than 1KiB | Retains Gorilla and the existing protocol; no shared WebSocket dictionary |
| T3 shell | Emit existing project and active-thread updated/removed events | Archive transitions and ordering that upserts cannot reproduce use snapshots; newer clients ignore archive updates and remove records from both locations |
| T3 thread | Replace changed thread, node, runtime-request, message, and turn-item rows | Run-state changes, structure, visibility, order, and unsupported fields fall back to snapshots |
| Opt-in snapshot compaction | With boolean `acceptCompactTurnItems: true`, omit the duplicated local visible prefix from wire `turnItems` and set `turnItemsOmitLocalVisible: true` | Retain `visibleTurnItems` and the complete internal projection; legacy clients receive full snapshots. The client prepends local visible items before applying reducers. No body deduplication or run-event changes |
| T3 cursors | Track committed Core coverage separately from strictly increasing per-subscription wire sequences | One Core event may produce several row events. Only Core coverage filters incoming events; fallback snapshots advance the wire sequence. Reconnect always establishes a fresh epoch with an authoritative snapshot, never replaying a wire cursor against Core |
| User message timestamps | Use the submitted run's creation time for its user message and user turn item | Assistant output no longer creates false user-row changes; this corrects client-visible timestamp semantics |
| T3 send selection | Compare serialized delta and snapshot sizes and send the smaller representation; skip unchanged state | Each subscription retains its previous projection, consuming proportional memory; dirty ticks still rebuild the projection |
| Covered events | Use the original subscription snapshot for the first frame; ignore queued events already covered by a published snapshot | Avoids redundant work when projection construction overlaps event delivery |
| Native JSON and NDJSON | Negotiate gzip BestSpeed for snapshot, workbench, and event responses | Clients without gzip retain their existing response format; handles Vary, Content-Length, and streaming Flush |
| Event batching | Keep one gzip dictionary per response; flush at 4KiB or 8ms after the first pending event | 4KiB is a trigger, not an event truncation limit; adds up to approximately 8ms of batching delay, excluding scheduling and backpressure |
| Local Unix clients | Disable automatic compression negotiation and retain immediate event delivery | Avoids unnecessary local CPU and delay; the remote proxy preserves explicit encoding negotiation |
| LAN and native relay | Negotiate h2/http/1.1 inside TLS and enable server HTTP/2 with ServeTLS | Older peers fall back to HTTP/1.1; existing linkd byte relays can carry HTTP/2 without a protocol upgrade |
| Reconnection | Reuse a per-peer TLS session cache and retain idle client connections for five minutes | HTTP/2 still uses TCP and remains subject to connection-level loss recovery |
| Direct probes | Apply the 1.2-second per-address probe context to TLS handshakes as well as TCP dialing | A reachable TCP socket that stalls during TLS cannot delay fallback for the full handshake timeout |
| linkd HTTP pools | Isolate pools by route/channel; retain at most two idle connections per target for 30 seconds | Prevents reuse across devices sharing the synthetic HTTP host; preserves existing slow-request and long-stream timeout semantics |

T3 event schemas and reducer behavior were checked against these public interfaces:

- https://github.com/pingdotgg/t3code/blob/main/packages/contracts/src/orchestrationV2.ts
- https://github.com/pingdotgg/t3code/blob/main/packages/client-runtime/src/state/threads.ts
- https://github.com/pingdotgg/t3code/blob/main/packages/client-runtime/src/state/shellReducer.ts
- https://github.com/pingdotgg/t3code/blob/main/packages/client-runtime/src/state/orchestrationV2Projection.ts

HTTP/2 provides multiplexing, flow control, and per-stream cancellation. The implementation introduces no custom relay frame format, credit protocol, content digest, or persistent projection log. Legitimately idle subscriptions and control connections are not subject to a new global idle-disconnect timer.

## Validation

These are fixed synthetic fixtures and local integration tests, not production packet captures or reused measurements from an earlier assessment.

| Test | Observed result |
| --- | --- |
| Original 0.1.22 fixture: 41 runs, ten committed cursor positions | 443,332 B → 7,115 B was only a successful-path measurement. It failed to expose fallback at ordinary Core cadence and does not establish typical streaming savings |
| Corrected 41-run fixture, including cursor advances of 1, 2, and 3 | 443,332 B → 6,021 B in two assistant-row events; full state reconstruction matches the authoritative projection |
| Real temporary Core, controlled provider emitting every 20ms, identical text workload | Before: 78 snapshots, no row events, 1,209,295 B. After: 2 structural snapshots, 150 row events, 295,051 B (about 75.6% less). Payload bytes are decoded JSON, excluding WebSocket compression and TLS overhead; scheduling affects exact counts |
| Wire cursor ahead of Core | Repeated one-event changes remain deliverable; thread and shell snapshot fallbacks never regress within a subscription. Reconnect resets from a complete snapshot and subsequent events remain deliverable |
| Opt-in compact snapshot, 41-run fixture | Projection envelope: 443,299 B → 303,454 B (31.5% less before compression). The upstream `restoreLocalVisibleTurnItems` function reconstructs the original state, including a separate fixture with large tool output, reasoning, approval, queued input, and steering |
| Compact subscription lifecycle | Boolean capability negotiation, legacy and false/string offers, prefix/body/order rejection, initial/fallback snapshots, subsequent deltas, and reconnect preserve full state |
| 600 synthetic NDJSON events | 111,982 B becomes 4,118 B with gzip, or 3.7%; decompressed bytes match the complete original stream |
| Sparse compressed event | Arrives while the stream remains open; cancellation releases the subscription |
| Native HTTP/2 client | One persistent subscription and three Health requests use one TLS connection |
| Legacy HTTP/1.1 peer | The same workload uses two connections and preserves concurrent subscription/request operation |
| Real linkd byte tunnel | One persistent subscription and four requests use one end-to-end TLS tunnel with inner HTTP/2 |
| linkd HTTP pools | Nine requests across three route/channel targets open three tunnels without crossing response or encoding boundaries |
| T3 socket | Compression negotiation, initial snapshot, incremental reconstruction, increasing cursors, covered-event suppression, reconnect, and authorization revocation |
| Manager TLS endpoint | Negotiates HTTP/2 and forwards client encoding negotiation to the Unix backend |

Verification commands:

```sh
# Vantry
go test -race ./internal/t3compat ./internal/remoteaccess \
  ./internal/adapters/transport/local ./internal/adapters/transport/sshrelay \
  ./internal/connectivity

# linkd
go test -race ./...
```

Process checks confirmed no remaining test, compiler, or SSH child processes after these checks.

## Upgrade compatibility

Vantry must be updated to enable its HTTP/2, compression, and T3 projection changes. Updating linkd enables HTTP ingress connection pooling; its existing byte-tunnel protocol already carries inner HTTP/2. New clients retain HTTP/1.1 compatibility with older Vantry servers. Release and deployment status is recorded separately from implementation validation.
