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

- Every transaction that mutates any key related to an object — version
  keys (`:V:`), child keys (`:C:`), or the object entry itself — first
  acquires a **claim on the primary key** (`:O:`), via a single shared
  helper function that is the *only* way to obtain such a transaction
  (`withObjectTxn(ctx, objectKey, fn)`). The mechanism differs by backend
  (see §6), but the invariant is the same: it is structurally impossible to
  mutate object state without the claim — enforced by the API surface, not
  by convention.
- Any transaction that is about to mutate a *specific* version key
  (delete-target, promote-target) also pessimistically locks that specific
  key before writing it (required regardless — see §4).

This trades some unnecessary serialization (e.g. two deletes of two
different non-current versions now queue behind the same claim even
though they didn't need to conflict) for removing an entire class of future
bugs. Reasonable default unless profiling shows contention cost is a real
bottleneck for a given object.

**Does the second worked example in §2 invalidate this?** No — but it does
clarify the scope. The example shows a legitimate mutation (delete of a
non-current version) that does *not* write the primary key and is still
correctly serialized, because it writes the specific `:V:` key it depends on.
This satisfies the general invariant above — the key the transaction depends
on *is* in its write set — just not through the primary key.

What the example does not do is justify skipping the primary key claim for
*other* operations. The structural fix is deliberately conservative: "always
claim the primary key" is a sufficient condition that is easy to enforce and
impossible to accidentally violate when adding new operations. The delete of a
non-current version is a case where the minimal necessary condition happens
to be provably weaker. For any operation that is not this narrow (anything
that reads `:V:` to *choose* which key to then act on, rather than acting on
a specific key supplied by the caller) the primary key claim is still
required, because the scan result itself is not in the write set.

**TiKV alternative** if profiling shows the lock's uncontended round-trip
cost is significant: stay optimistic, but make every mutating operation
*unconditionally* bump a monotonic generation counter co-located with the
primary key. This guarantees write-set intersection for any two concurrent
mutations without a blocking RPC — cheaper on the uncontended path, same
"hard to get wrong" property as the lock-based approach since the rule
("always bump the counter") is unconditional. On FDB this counter is not
needed — see §6.

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

## 5. FoundationDB comparison

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
  on data that is structurally immutable once written.

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

## 6. Backend mechanisms for the claim on `:O:`

The backends achieve the `:O:` claim differently. `withObjectTxn` encapsulates
the difference so that operation authors never interact with it directly.

### FDB

Every mutating operation reads `:O:` before making any routing decision —
to verify existence, obtain the `ref_tag`, or read the current version. On FDB
that non-snapshot read is the claim: it automatically registers a read-conflict
range on `:O:`. Because `PutObject` always writes `:O:`, any concurrent
`PutObject` is caught by the Resolver at commit regardless of which child or
version key the operation then writes. For operations that also scan `:V:` or
`:C:*`, FDB's `getRange` additionally registers a read-conflict range over the
scanned subspace, providing phantom protection for free.

No extra coordination key is needed on FDB. `withObjectTxn` just ensures its
`:O:` read is non-snapshot:

```
// FDB
func withObjectTxn(txn, objectKey, fn):
    obj = txn.Get(objectKey)   // non-snapshot — registers read-conflict on :O:
    return fn(txn, obj)
```

### TiKV

On TiKV a read registers nothing. Two options:

**Pessimistic lock** — `LockKeys(:O:)` before any read or write. The claim is
proactive: concurrent writers on the same object block until the lock is
released, so the losing transaction never does wasted work.

```
// TiKV — pessimistic lock
func withObjectTxn(txn, objectKey, fn):
    txn.LockKeys(objectKey)
    obj = txn.Get(objectKey)
    return fn(txn, obj)
```

**Epoch counter** — unconditionally read and increment a coordination key
co-located with `:O:`. Optimistic: no blocking RPC, no deadlock risk, but
conflict is detected only at commit and the losing transaction's work is
discarded.

```
// TiKV — epoch counter
func withObjectTxn(txn, objectKey, fn):
    gen = txn.Get(epochKey(objectKey))
    obj = txn.Get(objectKey)
    txn.Put(epochKey(objectKey), gen + 1)
    return fn(txn, obj)
```

Both guarantee that any two concurrent mutations on the same object
conflict — whichever `withObjectTxn` variant is chosen, one will be forced to
retry with a fresh view of `:O:`. The choice between them is a TiKV-specific
performance tradeoff (see §3).

### Why the epoch counter write is not needed on FDB

If the epoch counter pattern were applied on FDB, the write would be redundant.
FDB's Resolver checks read-conflict vs. write-conflict, not write-write: two
transactions that both write a key without reading it do not conflict. Writing
`:E:` on FDB causes a conflict only when a concurrent transaction has a
read-conflict range covering it — but the non-snapshot `Get(:O:)` already
covers `:O:` directly, making the epoch key dead weight. On TiKV the epoch
write is the only available mechanism for forcing write-set intersection.

## 7. Declarative operation API

All the analysis in §2, §3, and §6 — write-set coupling, epoch keys,
retry-changes-case behaviour — arises because RGW
constructs transactions at the KV primitive level and must therefore understand
each backend's isolation model. The alternative is to move that boundary up:
RGW expresses *what* it wants to happen, and each backend implementation is
fully responsible for *how* to do it safely.

### The interface

```
Put(ctx, key, value) → versionId
DeleteCurrent(ctx, key) → versionId
DeleteVersion(ctx, key, versionId) → error
GetCurrent(ctx, key) → (value, versionId)
GetVersion(ctx, key, versionId) → value
ListVersions(ctx, key, ...) → []version
```

All of the versioning operations discussed in this document are single-object,
single-transaction operations and map cleanly onto this interface. Behind it,
the TiKV backend owns the epoch key and the `withObjectTxn` helper. The FDB
backend uses read-conflict ranges
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

## 8. Child-key operations and the limits of write-set intersection

The write-set intersection technique in §2 works because of a directional
property: every operation that could invalidate a concurrent read necessarily
writes the same anchor key (`:O:`). The intersection is not arranged; it is
forced by what the operation must do. You cannot change "current" without
writing the primary pointer.

Child-key operations like `PutObjectTagging` expose a case where this
structural property does not exist — and cannot be made to exist without
abandoning the write-set approach entirely.

### The scenario

When tags spill to an external child entry (`:C:<ref_tag>T`), a subsequent
`PutObjectTagging` call (where the object is already external — the common
case after the first call) reads `:O:` to verify the object exists and to
obtain the `ref_tag`, then writes only `:C:<ref_tag>T`. It has no legitimate
write to perform on `:O:` — the object's identity, content, and metadata are
unchanged.

Concurrent `PutObject` replaces the object: new `:O:` with `ref_tag=B`, old
`:O:` moved to GC.

Write sets:
- `PutObjectTagging`: `{ :C:A:T }`
- `PutObject`: `{ :O:, G:... }`

No intersection. On TiKV, `PutObjectTagging` commits successfully, writing
tags to a `ref_tag` that is now in GC. The GC entry was written before
`:C:A:T` existed, so the GC worker has no record of it to clean up. The tags
are silently orphaned; the new `:O:` (with `ref_tag=B`) shows no external
tags.

On FDB, `PutObjectTagging`'s `Get(:O:)` registers a read-conflict range on
`:O:`. `PutObject`'s write to `:O:` carries a write-conflict. The Resolver
fires, `PutObjectTagging` aborts and retries, reads the new `:O:` with
`ref_tag=B`, writes `:C:B:T`. Correct.

### Why write-set intersection cannot be the solution

The technique in §2 requires the *invalidating* operation — the one that makes
your read stale — to write some key that you also write. For the version chain,
the invalidating operation is `PutObject`, which writes `:O:`; and every
version-mutating operation also writes `:O:`, so the invariant holds.

For `PutObjectTagging`, the invalidating operation is `PutObject`, which writes
`:O:`. `PutObjectTagging` writes `:C:<ref_tag>T`. There is no key that both
have a semantic reason to write. Any intersection would have to be artificial —
forcing `PutObjectTagging` to also write `:O:` for no reason other than
satisfying the conflict-detection mechanism.

The epoch-counter approach from §3 would do exactly this: make `PutObjectTagging`
unconditionally bump a counter co-located with `:O:`. This creates intersection
mechanically and would protect the race. But it converts the guarantee from
structural to conventional. The rule "every child-key write must also bump the
epoch counter" cannot be derived from what the operation does — it is purely a
coordination protocol. A future operation that writes a child key and forgets to
bump the counter silently breaks isolation with no indication that anything is
wrong.

The version-chain invariant ("does this operation change 'current'? then it
writes `:O:`") is semantically verifiable — you can look at an operation and
determine whether it applies. The child-key invariant ("does this operation read
`:O:` to route a child-key write? then it must also bump `:E:`") is not
verifiable from semantics; it can only be enforced by structure.

### Implication

For child-key operations, the pessimistic lock via `withObjectTxn` is the only
robust solution on TiKV. The read of `:O:` must be covered by a lock acquired
before any decision about which child key to write; write-set intersection after
the fact cannot substitute for it, because no natural write-set overlap exists.

This also tightens the scope of the epoch-counter alternative described in §3.
The epoch counter is viable for version-chain mutations — operations on that
family naturally cluster around `:O:`, and the "always bump the counter" rule
maps cleanly onto "always write `:O:` for semantic reasons anyway". It is not
viable as a replacement for the pessimistic lock on child-key operations, where
the bump has no semantic grounding and the class of operations that must carry
it can only be identified structurally.

The declarative API in §7 absorbs this automatically: `withObjectTxn` lives
inside the backend, and both version-chain mutations and child-key operations
go through it. The distinction between the two cases is an implementation detail
of the backend, invisible to RGW.

