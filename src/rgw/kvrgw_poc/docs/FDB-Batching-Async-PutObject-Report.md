# How FDB Batching and Async Are Used in `put_object`

Paths below are relative to `src/rgw/kvrgw_poc/`.

## 1. End-to-end request path

`KvRgwServiceImpl::put_object()` (`backend/src/service_impl.cpp:2328`) builds the
`ObjectValue` (ref_tag, etag, timestamps) and calls `put_object_route()`
(`service_impl.cpp:2415`), which branches on `tier_config.batch_size`:

- **`batch_size == 1`** (batching disabled, the default): the object is classified
  by size and routed to either `put_object_single_txn()` (`service_impl.cpp:2206`,
  inline/zero-byte objects) or `select_storage_tier()` (`service_impl.cpp:2270`,
  `CHUNK_STORAGE`/`CHUNK_CHILD_D` tiers for larger objects).
- **`batch_size > 1`**: a `BatchCommitEntry` is built and handed to
  `batch_queue_.enqueue()` (`service_impl.cpp:2461` → `service_impl.hpp:120`),
  and the caller blocks on the returned `std::future<BatchPutResult>`. Batching
  is otherwise transparent to the caller — same return type, added latency only.

For the non-batched `CHUNK_STORAGE` path, `select_storage_tier()` runs the
three-phase protocol documented in `docs/PutObject-CHUNK_STORAGE-Protocol-Spec.md`:

1. **Phase 1** — unconditional `store_.set(po_key, po_value)`, no transaction (`service_impl.cpp:2303`).
2. **Phase 2** — `data_store_.write(ref_tag, data)`, blob written to the filesystem, not FDB (`service_impl.cpp:2310`).
3. **Phase 3** — `put_object_phase3()` (`service_impl.cpp:1823`), a retried FDB
   transaction built from `put_prepare()` (`service_impl.cpp:1957`) +
   `put_finalize()` (`service_impl.cpp:1983`), retried up to 10 times with
   linear backoff (`10ms * (attempt+1)`) on FDB-retriable errors.

Only Phase 3 (or the batch's shared Phase 1/Phase 3, below) touches FDB
transactionally — the blob itself is never stored in FDB.

## 2. The async mechanism

This is not coroutines, seastar, or `boost::asio` — it's the raw FoundationDB
C API (`FDBFuture*`) wrapped in small RAII types and advanced by **polling**,
not blocking per-request threads:

- `FdbFuture` (`backend/src/kv_store.hpp:39`) — move-only wrapper around
  `FDBFuture*`; `is_ready()` (`kv_store.hpp:59`) is a non-blocking poll via
  `fdb_future_is_ready`.
- `KvTransaction::kv_async_get()` (`kv_store.cpp:248`) issues
  `fdb_transaction_get` and returns immediately — fire, don't wait.
- `kv_wait_get()` (`kv_store.cpp:257`) blocks until ready and copies the result
  into a `std::string`. This blocking+copying path is what the Zero-Copy Future
  refactor replaced on the PUT hot path (§4 below).
- Commits are async too: `commit_async()` (`kv_store.cpp:422`) returns
  immediately; `resolve_commit()` (`kv_store.cpp:427`) checks the error once the
  future is ready.
- The batched path (`BatchCommitQueue::run()`, `service_impl.cpp:263`) is a real
  poll-driven state machine: each of `batch_threads` worker threads (default 8)
  manages up to `kMaxInflight = 4` concurrent `InFlightBatch` slots
  (`service_impl.hpp:163`), each slot stepping through `PHASE_1` /
  `PHASE_3` × `STEP_WORKING` / `STEP_COMMIT_ISSUED`. A slot only advances when
  its `FdbFuture`/commit future polls ready; otherwise the worker sleeps
  ~50–100µs and checks the other slots (`service_impl.cpp:267–494`).

## 3. What "batching" actually means here

Two distinct things, both covered in `docs/batch_mode.md`,
`docs/batch_put_design.md`, and Part III (§19–26) of the protocol spec:

**(a) Multiple independent PUT requests coalesced into one FDB transaction.**
`tier_config.batch_size` controls how many `BatchCommitEntry`s are pulled from
the shared `pending_` deque (`service_impl.hpp:175`) into one batch. A batch is
cut once `pending_.size() >= batch_size` **or** the oldest entry has waited
`>= batch_timeout_us` (`service_impl.cpp:263–470`). Entries targeting the same
object key within a batch are pushed to the back of the queue to avoid
intra-batch write conflicts (`service_impl.cpp:435–453`).

**(b) Pipelined/parallel async reads within a single transaction.** Both the
single-object path and the batch path issue *all* `async_get()` calls for a
transaction up front, then wait on them — one network round trip services N
reads. In `put_prepare()`/`put_finalize()`, the bucket read, object-row read,
and (for storage tier) the `P:O`-existence read are all issued before anything
is awaited. In the batch's `do_phase3_work()` (`service_impl.cpp:583`), this is
scaled to up to `kMaxBatchSize = 16` entries (`constants.hpp:76`) plus
deduplicated bucket reads and one shared group read — again, all issued before
any `wait()` (`service_impl.cpp:606–669`).

Batch-mode phases (protocol spec §19–26):

- **Phase 1** (storage-tier entries only): one shared "group `P:O`" key listing
  every storage-tier `(ref_tag, size)` pair in the batch, replacing up to
  `batch_size` individual Phase-1 writes with a single commit
  (`start_batch()`, `service_impl.cpp:496`).
- **Phase 2**: still per-entry and sequential — blob writes go to the
  filesystem, not FDB, so there's nothing to batch here.
- **Phase 3**: one shared FDB transaction, pipelined reads, per-entry
  `put_finalize()`, one shared group-`P:O` delete, one `commit()` for the whole
  batch. A transaction-level failure fails every member of the batch
  identically; success resolves each entry's own `std::promise<BatchPutResult>`
  independently (`service_impl.cpp:341–375`).

Note: `BatchCommitQueue::commit_batch()` (`service_impl.cpp:735`, declared
`service_impl.hpp:168`) is a second, simpler implementation of the same
commit logic but is dead code — unreferenced outside its own definition. The
live runtime path is `run()` → `start_batch()` → `do_phase3_work()`.

## 4. Zero-Copy Future refactor (commit `4f7714601e4`)

`docs/zero_copy_future.md` describes an earlier design sketch (marked "not
implemented" against the old `FdbFuture` type). What actually landed is
`FdbGetHolder` (`kv_store.hpp:78`), which owns an `FdbFuture` and exposes
`wait()` / `present()` / `value()`. `value()` returns a `std::string_view`
pointing directly into FDB's own result buffer (`kv_store.hpp:97`), set by
`fdb_future_get_value` inside `FdbGetHolder::wait()` (`kv_store.cpp:309`) —
no `std::string` allocation or copy.

The commit replaced PUT/COPY call sites that used the blocking,
copy-into-`std::string` `kv_wait_get()` with `FdbGetHolder` at every read in
`put_finalize()` (bucket-verify get, existing-object get, pending-marker get)
and in `copy_object()` (`service_impl.cpp:1990`, `1576`, etc.).

This doesn't change *what* is batched or *when* reads are issued — the
pipelined issue-then-wait-all-later pattern is unchanged. It removes a
per-get heap allocation + memcpy from the hot path, which matters more under
batching since `do_phase3_work()` resolves up to 16 per-entry gets plus a
group get per committed batch.

## 5. Config / tuning knobs

| Knob | Default | Effect | Source |
|---|---|---|---|
| `KVRGW_BATCH_SIZE` (`tier_config.batch_size`) | 1 (batching off) | entries coalesced per FDB transaction | `tier_config_state.cpp:113` |
| `KVRGW_BATCH_TIMEOUT_US` | 1000 | max wait before flushing a partial batch | `tier_config_state.cpp:116` |
| `KVRGW_BATCH_THREADS` | 8 | worker threads running `BatchCommitQueue::run()` | `tier_config_state.cpp:119` |
| `kMaxInflight` | 4 | concurrent in-flight batches per worker thread | `service_impl.hpp:163` |
| `kMaxBatchSize` | 16 | hard cap on entries per batch (sizes stack-allocated arrays) | `constants.hpp:76` |
| Admin socket `set-tier-config batch_size=...` | — | runtime reconfiguration | — |

Phase-3 retry policy (applies with or without batching): up to 10 attempts,
linear backoff `10ms * (attempt + 1)` on FDB-retriable errors.

## 6. Summary

- FDB access throughout is non-blocking-issue / poll-to-complete via
  `FDBFuture`, never a blocking call-per-request model.
- "Batching" has two independent meanings in this codebase: (1) coalescing
  several client PUTs into one shared FDB transaction (controlled by
  `batch_size`/`batch_timeout_us`), and (2) pipelining multiple reads within
  a single transaction regardless of batching (always on, via
  `async_get`/`FdbGetHolder`).
- The blob payload itself is never part of FDB batching — it's written to the
  filesystem in Phase 2, independent of transaction/batch boundaries.
- Zero-Copy Future is an efficiency refactor of the read path (avoids
  string copies), orthogonal to the batching logic but compounding its
  benefit since batched Phase 3 resolves many more reads per transaction.
