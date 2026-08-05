# TiKV / FDB transaction semantics for object versioning

Notes from working through how to safely implement a delete-current-version
operation (primary key → current version pointer, separate version keys) on
top of TiKV's transactional KV API, with a comparison to FoundationDB.

## 1. TiKV's core conflict-detection mechanism

TiKV transactions (Percolator model) give **snapshot isolation**, not
serializability, by default:

- Reads see a consistent MVCC snapshot as of the transaction's `start_ts`.
- At commit, TiKV only checks for write-write conflicts on keys in your
  **write set** (the keys you `Prewrite`) — it checks whether a commit
  already landed on that key with `commit_ts > start_ts`.
- Keys you only `Get`/`Scan` are invisible to this check. A plain range scan
  gives you **no protection** against concurrent writers modifying or
  inserting keys in that range.
- TiKV has **no gap/predicate locking** at all — even under pessimistic
  locking, you can only lock keys that exist. A concurrent insert into a
  "locked" range is never blocked, because there's no lock covering the gap.

## 2. The core technique: piggyback correctness on a shared write key

Since TiKV won't protect a scan directly, the working pattern is to arrange
for **every transaction that could invalidate your scan's result to
necessarily write a key you're also writing** — then TiKV's ordinary
write-write conflict check catches the race for you.

For the versioning scheme: any operation that changes what "current" points
to (a new upload, a delete-of-current recomputing the next version) writes
the **primary key**. Two such operations racing will always collide on that
key, so TiKV's default conflict check protects the scan transitively — even
though the scan itself was never locked.

Worked example:

- State: `v1(ts10)`, `v2(ts20)`, `v3(ts30)`, primary → `v3`
- Txn A (delete v3): `start_ts=100`. Reads primary=v3, scans, computes
  next=v2. Prewrites: delete(v3), write(primary=v2).
- Txn B (upload v4), concurrently: `start_ts=105`. Writes v4, writes
  primary=v4. Commits at `commit_ts=110`.
- A's commit fails: `commit_ts=110 > A.start_ts=100` on the primary key →
  `WriteConflict`. A must retry.
- **On retry, A must recompute from scratch** (fresh read of primary, fresh
  scan) — not blindly resubmit its old computed value, or it will write a
  stale/wrong pointer.

Second worked example — conflict on a `:V:` key, not the primary key:

- State: primary → `v3`, version chain: `v3(ts30)` current, `v2(ts20)` and
  `v1(ts10)` in `:V:`.
- Txn A (delete current v3): `start_ts=100`. Reads primary=v3 (matches target
  → Case 2). Scans `:V:`, finds v2. Prewrites: delete(`:V:v2`), write(primary=v2),
  move v3 to GC.  Write set: `{primary, :V:v2}`.
- Txn B (delete non-current v2), concurrently: `start_ts=105`. Reads primary=v3
  (no match → Case 1). Reads `:V:v2`. Prewrites: delete(`:V:v2`).
  Write set: `{:V:v2}`. Commits at `commit_ts=110`.
- A's commit fails: `commit_ts=110 > A.start_ts=100` on `:V:v2` →
  `WriteConflict`. A must retry.
- On retry, A re-reads primary=v3 (B never touched it — still Case 2), rescans
  `:V:` → finds only v1. Promotes v1 instead. Correct.

Two things to note here compared to the first example:

1. **The conflict key is `:V:v2`, not the primary key.** B never wrote the
   primary key — it only deleted a non-current version. The §1 "piggyback on a
   shared write key" technique still works, but the shared key is determined by
   what A happens to promote, not by a single unconditional anchor key.

2. **Retry can change case.** Consider the symmetric outcome where A commits
   first (primary now points to v2). B retries, re-reads primary=v2 — which now
   *matches* B's target — and B flips from Case 1 to Case 2. B must then scan
   `:V:` for the next version and promote v1. An implementation that re-runs the
   Case 1 code path blindly on retry would fail to find v2 in `:V:` and return
   a wrong 404. Recompute from scratch means re-evaluating which case applies,
   not just re-executing the same case.

## 3. Why this needs an explicit, structural invariant — not a case-by-case rule

The failure mode: if some future operation can change what's "current" (or
depends on a specific version key continuing to exist) *without* writing the
key that ties it into the conflict check, the protection silently stops
applying — no error, just a race.

Example that breaks a naive version of this design: a "promote an old
version to current" operation only needs to read the target version key and
write the primary key. A concurrent delete of that *specific* non-current
version key (which never touches the primary key, since deleting a
non-current version doesn't change currency) can race it: promote reads v1
exists, delete removes v1, promote commits primary → v1 anyway (disjoint
write sets, no conflict detected) → dangling pointer.

**General invariant**: any transaction that depends, at commit time, on a
specific key's value or existence must include that key in its write set
(optimistic) or lock set (pessimistic) — reading it is never enough.

**Recommended structural fix**: don't rely on developers re-deriving, per
operation, which keys need to be touched. Instead:

- Every transaction that mutates an object's version state acquires a
  pessimistic lock on the **primary key** first, via a single shared helper
  function that is the *only* way to obtain such a transaction (e.g.
  `withObjectTxn(ctx, objectKey, fn)`). This makes it structurally
  impossible to write code that mutates object state without holding the
  lock — the invariant is enforced by the API surface, not by convention.
- Any transaction that is about to mutate a *specific* version key
  (delete-target, promote-target) also pessimistically locks that specific
  key before writing it (required regardless — see §4).

This trades some unnecessary serialization (e.g. two deletes of two
different non-current versions now queue behind the same primary lock even
though they didn't need to conflict) for removing an entire class of future
bugs. Reasonable default unless profiling shows the lock wait is a real
bottleneck for a given object.

**Does the second worked example in §2 invalidate this?** No — but it does
clarify the scope. The example shows a legitimate mutation (delete of a
non-current version) that does *not* write the primary key and is still
correctly serialized, because it writes the specific `:V:` key it depends on.
This satisfies the general invariant above — the key the transaction depends
on *is* in its write set — just not through the primary key.

What the example does not do is justify skipping the primary key lock for
*other* operations. The structural fix is deliberately conservative: "always
lock the primary key" is a sufficient condition that is easy to enforce and
impossible to accidentally violate when adding new operations. The delete of a
non-current version is a case where the minimal necessary condition happens
to be provably weaker. For any operation that is not this narrow (anything
that reads `:V:` to *choose* which key to then act on, rather than acting on
a specific key supplied by the caller) the primary key anchor is still
required, because the scan result itself is not in the write set.

Alternative if profiling does show contention cost: stay optimistic, but
make every mutating operation *unconditionally* bump a monotonic generation
counter co-located with the primary key, as part of its normal write set.
This guarantees write-set intersection for any two concurrent mutations on
the object without an extra blocking RPC — cheaper on the uncontended path,
same "hard to get wrong" property as the lock-based approach since the rule
("always bump the counter") is unconditional rather than semantic.

## 4. Pessimistic locking in TiKV — what it actually does

- Pessimistic transactions require you to `LockKeys` a key **before**
  `Set`/`Delete`-ing it — writing an unlocked key gives no consistency
  guarantee for that key.
- `LockKeys` sends a `PessimisticLock` RPC that proactively checks for
  conflicts and blocks/waits if the key is already locked, rather than
  deferring the check to commit time.
- At commit, pessimistically-locked keys go through `PrewritePessimistic`,
  which skips re-checking the Write CF (already validated at lock time).
  Keys written but never locked go through ordinary `Prewrite` — the same
  optimistic-style, commit-time-only check. **Both mechanisms coexist within
  a single pessimistic transaction** — this isn't a special case, it's how
  TiDB itself works (pessimistically locks unique-index keys, writes
  non-unique index keys without locking them).
- Cost: `LockKeys` is one extra network round trip compared to a pure
  optimistic transaction (which pays nothing extra — the conflict check
  rides inside the `Prewrite` you were sending anyway). If the locked keys
  span multiple regions, the client sends one `PessimisticLock` RPC per
  region, not per key.
- No deadlock risk is avoided for free — pessimistic locks can deadlock;
  TiKV runs deadlock detection to resolve this.

## 5. TiDB / SQL layer

TiDB compiles rows directly to TiKV keys (`t{tableID}_r{handle}` for a row,
keyed off the primary key/handle; `t{tableID}_i{indexID}{value}` for index
entries). `SELECT ... FOR UPDATE` on a specific row is literally the same
`LockKeys` RPC as the Go client example, just issued by the SQL layer.

- DML statements (`UPDATE`/`DELETE`/`INSERT`) automatically take the
  pessimistic lock on affected rows as part of executing the statement —
  the SQL engine does what the Go example does manually with `LockKeys`
  before `Set`/`Delete`.
- TiDB does **not** support gap locking (confirmed against current docs) —
  same phantom-read limitation as raw TiKV, for the same underlying reason
  (no gap-lock primitive at the KV layer). TiDB's own docs state its SI
  isolation level cannot avoid write-skew anomalies.
- Gotcha: when a DML statement contains an embedded `SELECT`, TiDB does
  **not** lock the rows read by that subquery — different from MySQL/InnoDB
  behavior, easy to trip over when porting logic.

## 6. FoundationDB comparison

FDB's default `get`/`getRange` behavior registers a **read-conflict range**
for every read automatically — so a range scan *is* protected by default,
without needing the "co-write a shared key" trick TiKV requires.

- Mechanism: at commit, a Resolver checks whether anything in your
  registered conflict ranges was written by a transaction that committed
  after your read version. This gives true serializability, including
  phantom protection (an insert landing inside a scanned range is caught).
- Explicit API for this: `AddReadConflictRange(begin, end)` /
  `AddWriteConflictRange(begin, end)` (plus single-key variants) let you
  register a conflict range **without actually performing the read** —
  e.g. you can claim the whole version-key subspace for an object as a
  conflict range even if you only intend to write one or two keys.
- **Still fundamentally optimistic** — there is no blocking/waiting
  mechanism, no server-side mutex. Conflict ranges are just metadata
  attached to the transaction; the check happens once, at commit, at the
  Resolver.
- **The Resolver checks read-conflict vs. write-conflict only — not
  write-write.** Two transactions that both write the same key without
  reading it do not conflict on FDB. This is the inverse of TiKV, which
  checks write-write and provides no read protection. Concretely:
  `AddWriteConflictRange` alone on a key provides zero protection if no
  other transaction reads that key; it only causes a conflict when a
  concurrent transaction has a *read*-conflict range covering that key.
- **Snapshot reads** (`snapshot=true` in the C API) perform a `Get` or
  `GetRange` without registering a read-conflict range. Useful for reads
  on data known to be immutable — e.g. walking deleted `:V:` entries in
  the prev-pointer scheme (§8) where re-creation is structurally
  impossible.

Comparison with TiKV pessimistic locking:

| | TiKV pessimistic `LockKeys` | FDB conflict range |
|---|---|---|
| Conflict detection | Proactive, blocking RPC at lock time | Only at commit, free (rides on commit message) |
| Loser's fate | Blocks/queues, then proceeds with fresh data | Aborted immediately, must redo entire transaction |
| Uncontended cost | One extra round trip always | Zero extra cost |
| Contended cost | Cheap — loser just waits | Expensive — loser's prior work is wasted |
| Range/phantom protection | None (key-only) | Full (any range, including inserts into gaps) |
| Deadlocks | Possible; needs detection | Impossible — no locks exist |

Practical implication: for a low-contention workload (rare concurrent
writers to the same object, which is the expected case here), FDB's
approach is likely both cheaper and safer by default. It degrades worse
than TiKV's pessimistic locking specifically under sustained high
contention on the same range, since FDB retries discard real completed
work rather than just dequeuing.

Also relevant: FDB's ~5-second transaction limit exists because Resolvers
only retain enough recent commit history to check conflict ranges within
that window.

## 7. S3 multipart uploads and versioning

A completed multipart upload produces exactly one object version, same as
a regular PUT:

- The version ID is assigned at `CompleteMultipartUpload`, not per part.
- `UploadPart` calls don't create versions — they stage data under an
  upload ID that isn't yet an object.
- An aborted or abandoned multipart upload creates no version at all.
- Concurrent multipart uploads to the same key are independent; whichever
  completes last wins as current (same last-writer-wins semantics as two
  concurrent regular PUTs).

Design implication: parts should live in a separate staging keyspace keyed
by upload ID until `CompleteMultipartUpload`, at which point the assembled
object is written as a version key + primary key update inside a single
transaction, going through the same locking path as a normal put. This
keeps the object-level lock held only for the atomic "publish" step, not
for the whole upload duration.

## 8. Eliminating range scans with a prev pointer

The range scan in Case 2 (delete current, find next to promote) is the source
of all the complexity in §2 and §3. It can be eliminated entirely by storing
`prev_version_id` in every `:O:` and `:V:` entry — a direct pointer to the
version immediately below in the chain.

```
:O:     = { version_id=FFFC, ref=..., prev=FFFD }
:V:FFFD = { version_id=FFFD, ref=..., prev=FFFE }
:V:FFFE = { version_id=FFFE, ref=..., prev=FFFF }
:V:FFFF = { version_id=FFFF, ref=..., prev=nil  }
```

Case 2 becomes two point reads — `:O:` (already required) and
`:Get(:V:prev_version_id)` — with both in the write set. No range scan,
no implicit write-set coupling, no gap/predicate locking required.

### Stale pointers and the sequential walk

Case 1 (delete non-current) deletes its target `:V:` entry without updating
the prev pointer in `:O:`. If the deleted entry is the one `:O:`.prev
references, the pointer is stale.

Case 2 handles this by walking sequentially: if `Get(:V:prev_id)` returns
not-found, try `Get(:V:(prev_id + 1))`, and so on. This is safe because:

- Version IDs are strictly sequential integers with no gaps (concurrent PUTs
  serialize through `:O:`), so a not-found entry in the live ID range is
  definitively deleted — not absent because it was never created.
- Deleted entries are **write-once** — once removed they can never be
  re-created. Reading them without write-set protection is safe; their state
  is permanently fixed.
- The entry actually promoted is always in the write set. Write-write conflict
  (TiKV) or read-conflict range (FDB) protects it.

On FDB, intermediate `Get` calls on deleted entries register read-conflict
ranges that can never fire spuriously (the entries are immutable). As an
optimisation, snapshot reads (no conflict range registered) could be used for
those hops since immutability is guaranteed by design.

### Tradeoffs vs. range scan

The range scan approach handles gaps at the storage layer: TiKV's coprocessor
and FDB's range read both skip deleted entries internally and return the first
live key in one round trip regardless of how many gaps precede it. The
sequential walk pays one network round trip per gap.

| | Range scan | Prev pointer + walk |
|---|---|---|
| Case 2, no gaps | 1 range read + 1 point read | 2 point reads |
| Case 2, k gaps | 1 range read + 1 point read | k + 1 point reads |
| Case 1 | unchanged | unchanged |
| PUT | unchanged | unchanged (prev is the old `:O:` version_id, already known) |
| List versions | `:V:` range scan still required | `:V:` range scan still required |
| Storage | baseline | +4 bytes per entry |

Gaps form only when Case 1 concurrently deletes the specific version that
`:O:`.prev references. Lifecycle policies typically target old versions rather
than the most-recently-demoted one, so the common case is zero gaps and both
approaches cost the same. Under a workload that bulk-deletes recent non-current
versions concurrently with active Case 2 operations, the range scan has better
and more predictable performance.

On FDB, `get()` futures can be pipelined, recovering some per-hop latency if
reads are issued speculatively ahead of awaiting results. This does not fully
match a single range read and wastes bandwidth on the speculative requests that
turn out not to be needed.

The prev pointer design makes the TiKV conflict argument structural and obvious
at the cost of Case 2 read amplification proportional to concurrent gap-creating
Case 1 operations. The range scan has consistent O(1) Case 2 performance at the
cost of the implicit write-set coupling described in §2 and §3.

## 9. DB-agnostic range scan protection

If the range scan is kept (rather than replaced by the prev pointer scheme in
§8), the conflict protection must be made explicit and structural rather than
relying on incidental write-set intersection. The two backends require
genuinely different mechanisms.

### Why the epoch key write can be elided on FDB

In the generation-counter pattern discussed in §3, each mutating transaction
both reads and writes a per-object coordination key (`:E:objectname`):

```
gen = txn.Get(":E:objectname")   // registers read-conflict range on FDB
txn.Put(":E:objectname", gen+1)  // in write set on TiKV; write-conflict on FDB
```

The read is load-bearing on FDB. Without it, no read-conflict range is
registered on `:E:`, and FDB's Resolver — which checks read-conflict vs.
write-conflict only — would never fire on that key regardless of what other
transactions write there.

However, the Put is not what protects the `:V:` range scan on FDB. The
`getRange` call itself automatically registers a read-conflict range covering
the entire scanned subspace. Any concurrent transaction that writes into that
range carries a write-conflict on the key it modified; the Resolver detects the
overlap and aborts the scanner. The epoch key's write-conflict range plays no
part in this.

Consequently, on FDB you could elide not just the Put but the epoch key
entirely — the `getRange` read-conflict is sufficient. On TiKV the epoch key
(both read and write) remains required, because TiKV's write-write conflict
check is the only available mechanism and range scans are unprotected.

### The correct DB-agnostic abstraction

Because the backends have fundamentally different conflict models, the
abstraction should reflect that rather than using a single coordination key
that is load-bearing on one backend and dead weight on the other:

```
// FDB: getRange already registers a read-conflict range — nothing extra needed.
func protectVersionChainScan(txn, objectName):
    pass  // no-op

// TiKV: read+write epoch key unconditionally to force write-set intersection.
func protectVersionChainScan(txn, objectName):
    gen = txn.Get(epochKey(objectName))
    txn.Put(epochKey(objectName), gen + 1)
```

`withObjectTxn` calls `protectVersionChainScan` unconditionally before
invoking the caller's logic. On FDB the overhead is zero. On TiKV it is one
read-write round trip on the epoch key, which is the minimum cost of making
the range scan safe.

The key property of this abstraction: it is structurally impossible to perform
a version-chain mutation through `withObjectTxn` without the correct protection
being applied for the backend in use. The invariant is enforced by the API
surface, not by each operation author remembering which keys to touch.

## 10. Declarative operation API

All the analysis in §2, §3, §8, and §9 — write-set coupling, epoch keys,
prev pointer walks, retry-changes-case behaviour — arises because RGW
constructs transactions at the KV primitive level and must therefore understand
each backend's isolation model. The alternative is to move that boundary up:
RGW expresses *what* it wants to happen, and each backend implementation is
fully responsible for *how* to do it safely.

### The interface

```
Put(ctx, key, value) → versionId
DeleteMarker(ctx, key) → versionId
DeleteVersion(ctx, key, versionId) → error
GetCurrent(ctx, key) → (value, versionId)
GetVersion(ctx, key, versionId) → value
ListVersions(ctx, key, ...) → []version
```

All of the versioning operations discussed in this document are single-object,
single-transaction operations and map cleanly onto this interface. Behind it,
the TiKV backend owns the epoch key, the `withObjectTxn` helper, and the choice
between range scan and prev pointer. The FDB backend uses read-conflict ranges
internally. RGW sees none of it. The correctness argument — the entire subject
of this document — moves into the backend, which is the only place that knows
its own isolation model.

The structural invariant from §3 becomes trivially enforceable: `withObjectTxn`
lives inside the backend, not in RGW. Future backend authors write to the
declarative contract and cannot accidentally bypass conflict protection, because
RGW never had access to the KV primitives in the first place.

### Conditional operations

S3 conditional writes (`If-None-Match: *`, `If-Match: <etag>`) require the
condition check and the write to be atomic. These extend naturally as richer
operations rather than as leaks of transaction primitives:

```
PutIfAbsent(ctx, key, value) → (versionId, error)
PutIfVersion(ctx, key, matchVersionId, value) → (versionId, error)
```

Each is a single backend transaction with the condition check inside. The
interface remains declarative; the backend chooses how to implement the
atomicity.

### Cross-key operations

CopyObject and CompleteMultipartUpload require reads and writes that span more
than one object. A purely per-object declarative API cannot express these
without either:

1. **Adding them to the interface** — `CopyObject(ctx, srcKey, dstKey)`,
   `CompleteMultipartUpload(ctx, uploadId, key)`. The backend implements
   each as a single transaction where the backend supports cross-key atomicity,
   or as a read-then-write with a version check where it does not. These are
   S3-shaped concepts, not KV concepts leaking upward, so they are natural
   additions rather than abstraction violations.

2. **Accepting the documented consistency gap** — CopyObject at the S3 level
   does not guarantee that the source is unmodified between the read and the
   write. A read-then-write implementation is S3-compliant; backends that can
   do better atomically can do so transparently.

Exposing a transaction handle for cross-key reads would leak implementation
detail back through the interface and should be avoided.

### Testing benefit

A test suite for RGW can run against a trivial in-memory backend:

```
type MemBackend struct { mu sync.Mutex; objects map[string][]version }

func (b *MemBackend) DeleteVersion(ctx, key, versionId) error {
    b.mu.Lock(); defer b.mu.Unlock()
    // 10 lines of straightforward list manipulation
}
```

Concurrency correctness is tested separately against each backend's
implementation, not through the full RGW stack. The two concerns — S3 operation
semantics and KV transaction safety — are tested independently against their
respective interfaces.

## 11. TiKV storage layout (RocksDB)

- TiKV runs **two RocksDB instances** per node: one for KV data (`kvdb`),
  one for Raft logs (`raftdb`). Locks live in the KV instance, not the Raft
  one.
- Within `kvdb`, data is split into column families (CFs) — logical
  partitions within the **same** RocksDB instance, not separate databases:
  - **Default CF**: actual values
  - **Write CF**: MVCC version/commit metadata
  - **Lock CF**: pessimistic locks and prewrite locks
- CFs share the same WAL (so a lock write and a later commit write can be
  part of the same atomic, Raft-replicated write batch) but have separate
  memtables and SST files, so high-churn Lock CF traffic doesn't intermix
  with long-lived Default CF value data.
- Every read checks Lock CF first (fails if a lock is present), then looks
  up the latest valid commit in Write CF, then fetches the value from
  Default CF.
