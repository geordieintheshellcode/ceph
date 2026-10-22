# Single-Region PUT Phase 3

**Status:** proposal / investigation
**Scope:** `CHUNK_STORAGE` PutObject of a *new* object (no existing `O:` row),
both the single-put path and the batched path. Overwrites are explicitly out of
scope — see [§9](#9-out-of-scope-overwrites).

---

## 1. The problem

On TiKV, a transaction whose write set spans *N* regions is an *N*-participant
2PC: prewrite is fanned out to *N* region leaders, each Raft-replicating its
own prewrite, then the primary lock is committed and the secondaries are
resolved. One extra region in the write set costs an extra Raft round trip on
the latency-critical path, extra lock records, and an extra way for the commit
to fail or to leave locks behind for the lock resolver.

PUT Phase 3 always has **two** writes in its write set:

```
txn {
  ...
  put   (S | be16(sc) | be16(si) | be64(bucket_id) | 'O' | object_name)   // object row
  delete(P | 'O' | be64(bucket_id) | ref_tag)                             // pending marker
  commit
}
```

- object row — `make_object_key()`, `keys.cpp:174-179`; written at
  `service_impl.cpp:2034`
- pending marker — `make_po_key()`, `keys.cpp:260-265`; deleted at
  `service_impl.cpp:2037-2038`
- batched path: same two writes per entry, plus the group marker deleted at
  `service_impl.cpp:848-852`

The two keys differ in their **first byte** (`'S'` vs `'P'`), so they are at
opposite ends of the keyspace. They can never share a region. **Every
storage-tier PUT is unconditionally a 2-region 2PC**, for what is otherwise a
single-key write.

This is the single most common write in the system, so it is worth removing.

### 1.1 What is actually in each phase today

| Phase | Where | KV work |
|---|---|---|
| 1 | `service_impl.cpp:2234-2245` | bare `store_.set(P:O, PoEntry)` — no transaction |
| 2 | `service_impl.cpp:2248` | `data_store_.write(ref_tag, data)` — no KV at all |
| 3 | `put_object_phase3()`, `service_impl.cpp:1754` | txn: `get(B)`, `get(O:)`, `get(P:O)` → `put(O:)`, `delete(P:O)` |

The `P:O` value (`PoEntry`, `gc_value.hpp:89-93`) carries the `GcDescriptor`
(chunk type, `ref_tag`, `bucket_id`, `object_size`, **`mtime`**) plus the
object name — everything GC needs to free the blob without the `O:` row.

### 1.2 What the pending marker is for

Two distinct jobs, which is the key observation for this whole document:

1. **Crash recovery (durable job).** If the process dies between Phase 1 and
   Phase 3, the blob written in Phase 2 is referenced by nothing. The `P:O`
   entry is the only record that it exists. The sweeper
   (`sweeper.cpp:79-125`) scans the `P` namespace, and `move_po_to_go()`
   (`gc_value.cpp:195-235`) promotes aged orphans to `G:O` so the GC worker
   can free the blob.
2. **Concurrency control (transactional job).** Phase 3 reads `P:O` and
   deletes it in the same transaction. On FDB the read is a read-conflict
   range; on TiKV the `delete` is a write-write conflict. Either way it is
   mutual exclusion against a sweeper or a bucket-delete that is concurrently
   trying to reclaim the same marker
   (`Bucket-State-Management.md:185-189`, `:498`).

Job 1 does not require the marker to be touched by Phase 3 at all. Only job 2
does. **The proposal is to keep job 1 and pay for job 2 a different way.**

---

## 2. Constraints

Any fix has to respect the two properties that make the current layout good:

- **Listing.** `ListObjects` is a range scan of
  `[S | sc | si | bucket_id | 'O' | prefix, …)`. Nothing that is not a live
  object may appear in that range at a density that forces the scan to read
  and discard rows, or that breaks `max-keys` pagination accounting.
- **Sweeping.** The sweeper finds orphans with a *single* range scan of the
  whole `P` namespace (`make_p_prefix()` is one byte, `keys.cpp:274-279`),
  bounded by `kSweeperMaxKeys = 1000`. Orphans are rare, so in steady state
  that scan returns almost nothing and costs almost nothing. Any design that
  makes orphan discovery proportional to the number of *objects* rather than
  the number of *in-flight puts* is a non-starter.

These two constraints pull in opposite directions, which is why the obvious
answers do not work.

---

## 3. Alternatives considered and rejected

### 3.1 Co-locate the pending marker next to the object key

Make the pending key sort immediately after its own object key, e.g.
`S | sc | si | bucket_id | 'O' | object_name | 0x00 | ref_tag`. Two adjacent
keys are in the same region unless a split lands exactly between them, so
Phase 3 becomes effectively single-region with no protocol change at all —
the `get` + `delete` mutual exclusion of §1.2 job 2 is preserved verbatim.

**Rejected because of sweeping.** Pending markers would be scattered across
the entire object namespace. Finding orphans would require scanning every
object row in the cluster instead of a near-empty 1-byte prefix. That is the
non-starter from §2.

A weaker variant — a per-bucket pending category,
`S | sc | si | bucket_id | 'P' | ref_tag` — keeps listing clean (listing scans
`'O'`, not `'P'`) and keeps sweeping per-bucket, but it does **not** give
single-region: for any bucket large enough to span multiple regions, the
bucket's `'P'` sub-range is in the bucket's *last* region while the object key
is somewhere in the middle. It only works for small buckets, i.e. exactly the
case that does not need the optimisation.

### 3.2 Fold the marker into the object row

Phase 1 writes the `O:` key itself with an "uncommitted" flag, Phase 3
overwrites it with the real value. One key, one region, trivially.

**Rejected for three reasons:**

- Orphan discovery is the same scan-the-world problem as §3.1.
- The row is in the listing range while uncommitted, so every listing and
  every `HEAD`/`GET` has to filter on the flag, and `max-keys` accounting has
  to skip them.
- Phase 1 would have to clobber the existing row for an overwrite, which
  destroys the live object before the new data is durable. Overwrites are out
  of scope here, but a design that cannot be extended to them is not worth
  adopting for new writes only.

### 3.3 Leave it, rely on batching to amortise

The batched path already amortises the group marker over up to
`kMaxGroupPoSize = 8` entries (`constants.hpp:77`). But each entry still
writes its own `O:` row, and the group marker is still a second region, so
*every* batch commit is still ≥2 regions. Amortisation reduces the number of
extra participants per entry, never to one. It also does nothing for the
non-batched path.

---

## 4. Proposal: take the marker out of the commit

**Phase 3's write set becomes exactly one key: the object row.**

```
txn {
  get(B)    → bucket freshness / access / versioning   (read only, as today)
  get(O:)   → existing row                             (read only, as today)
  get(P:O)  → marker still present?                    (read only, as today)
  if now - P:O.mtime >= lease  → abort, retry/fail     (NEW — see §5)
  put(O:, new_value)
  commit                                               // single region
}
```

The `delete(P:O)` moves *after* the commit, as a non-transactional,
best-effort, batched clear:

```
on commit success:
  reply to the client immediately
  hand P:O key to the pending-reaper (in-process queue)
```

Reads are unchanged. `get(B)` and `get(P:O)` still touch other regions, but a
TiKV snapshot read is a single `Get` RPC to one replica with no lock, no Raft
round trip and no 2PC participation — it is not what makes this operation
expensive, and all three reads are already issued concurrently
(`put_prepare()`, `service_impl.cpp:1888-1907`).

### 4.1 The pending-reaper

An in-process queue of `P:O` keys whose objects have committed. A small
background thread drains it and issues batched clears.

This is cheap for a reason specific to this keyspace. The single-marker key is

```
'P' | 'O' | be64(bucket_id) | ref_tag        (22B, KeyHeaderPO, key_buf.hpp:103-113)
ref_tag = be32(rgw_id) | be64(seq)           (ref_tag.hpp)
```

`cat` precedes `bucket_id`, and `ref_tag` is *monotonic per RGW instance*. So
the markers produced by one RGW for one bucket are **strictly contiguous and
written in key order**. A reaper batch of *k* markers is therefore one or two
regions regardless of *k*, and the per-PUT cost of the delete drops by a factor
of *k* instead of costing a 2PC participant each time.

If the process dies with a non-empty queue, the markers are left behind and the
sweeper reclaims them exactly as it does today — no new failure mode, just a
slower path.

### 4.2 Nothing in the sweeper needs to change

`move_po_to_go()` already handles "marker whose object committed":

```cpp
if (*existing) {
  const auto object_value = parse_object_value(**existing);
  if (object_value && object_value->hdr.ref_tag == parts.ref_tag) {
    tr.kv_del(po_key.view());      // gc_value.cpp:217-223
    return 0;
  }
}
```

It reads `S:O`, sees a live row with a matching `ref_tag`, and deletes the
marker without creating a `G:O`. That is precisely the case the reaper is
racing it for, and both outcomes are identical and idempotent. The reaper is a
latency optimisation on top of a correct backstop, not a new correctness
dependency.

### 4.3 Batched path

Identical change in `do_phase3_work()`:

- keep step 4 (`service_impl.cpp:829-845`) — wait on every Phase 1 key and
  abort the batch if any is missing;
- add the lease check from §5 per Phase 1 key;
- delete step 5 (`service_impl.cpp:848-852`) — the `kv_del` loop over
  `ib.phase1_keys`;
- on commit success, push `ib.phase1_keys[0..phase1_key_count)` to the reaper.

The batch commit's write set becomes the *N* object rows of the batch and
nothing else. Those rows are in one region whenever the batch coalesces keys
from one bucket, which is the common case the batcher is built for.

---

## 5. Correctness

Removing `delete(P:O)` from the transaction removes the write-write conflict
that gave Phase 3 mutual exclusion against the sweeper on TiKV. That exclusion
has to be reconstructed. It is replaced with a **lease**.

### 5.1 The race

| | |
|---|---|
| T0 | Phase 1 writes `P:O` with `mtime = T0` |
| T1 | sweeper's scan snapshot: `P:O` present, aged ≥ `min_age`; reads `S:O` → absent |
| T2 | Phase 3 commits `put(S:O)` |
| T3 | sweeper commits `put(G:O)`, `delete(P:O)` |

Result: a live object row pointing at a blob queued for GC — silent data loss.
Today this is impossible: Phase 3 and the sweeper both write `P:O`, so one of
them aborts.

### 5.2 The lease

Phase 1 already stamps `mtime` into the `P:O` value
(`service_impl.cpp:2238`), and the sweeper already refuses to touch an entry
younger than `min_age_sec` (`sweeper.cpp:106-108`, default 60s,
`kvrgw_runtime.cpp:130`). Add the symmetric check on the writer side:

```
lease = min_age_sec - skew_margin
Phase 3 commits only if  now - P:O.mtime < lease
```

A sweeper only ever acts on markers older than `min_age_sec`; a Phase 3 only
ever commits on markers younger than `min_age_sec - skew_margin`. The two sets
are disjoint, so the §5.1 interleaving cannot occur. This is a lease, with the
usual caveat that it rests on bounded clock skew between the RGW that wrote
`mtime` and the sweeper, rather than on the KV store's conflict detection —
hence `skew_margin`. With `min_age_sec = 60`, a `skew_margin` of 5–10s leaves a
50s+ budget for Phase 2 + Phase 3, versus the single-digit milliseconds a
Phase 3 actually takes.

The `get(P:O)` read is **retained**, both as defence in depth and because it is
the only thing that detects a bucket-delete force-abort that reclaimed the
marker (`Bucket-State-Management.md:238-241`).

### 5.3 Long uploads

The lease is measured from Phase 1, and Phase 2 (the blob write) sits between
Phase 1 and the check. A multi-gigabyte upload can exceed `min_age_sec`, at
which point Phase 3 would refuse to commit a perfectly good object — and worse,
the sweeper would have already GC'd its blob.

Note this hazard **exists today**: the sweeper will promote the marker of a
still-uploading object to `G:O` once it passes `min_age_sec`, and today's
Phase 3 then finds `P:O` absent and fails with `INTERNAL`. The lease does not
introduce the problem, it makes it explicit and detectable before the commit
rather than after.

Fix: **renew the lease during Phase 2.** For an upload still running at
`mtime + lease/2`, re-`set` the `P:O` value with a fresh `mtime`. One
single-key non-transactional write to a region that is off the critical path,
at a frequency of at most one per 30s per in-flight large object.

### 5.4 Idempotence and crash matrix

| Crash point | Outcome |
|---|---|
| after Phase 1, before Phase 2 | marker aged out → sweeper → `G:O` → GC frees a blob that may not exist (already handled) |
| after Phase 2, before Phase 3 commit | marker aged out → sweeper → `G:O` → GC frees the orphan blob — **unchanged from today** |
| after Phase 3 commit, before reaper drains | marker outlives the commit; sweeper's `move_po_to_go()` sees live `S:O` with matching `ref_tag` → deletes the marker, no `G:O` (`gc_value.cpp:217-223`) — **this is the case the current code already handles** |
| reaper clear lost / reordered | same as above — the clear is idempotent and the sweeper is the backstop |

The "already-committed retry" branch of Phase 3
(`service_impl.cpp:1976-1993`) — marker present, existing row's `ref_tag`
matches ours → return `OK` — stays valid; it just stops being the only place
the marker is cleaned up.

### 5.5 What is given up

Honestly stated: an exact, store-enforced mutual exclusion is replaced by a
time-based one. On FDB the current scheme is airtight via read-conflict
ranges; on TiKV it is airtight via write-write conflict. The lease is airtight
only under bounded clock skew. The margin is three to four orders of magnitude
(seconds of skew budget versus milliseconds of transaction duration), and the
failure mode is detected (`abort` → the PUT fails and is retried by the
client), not silent. If that trade is unacceptable, §3.1's adjacent-key layout
is the alternative that keeps exact exclusion — at the cost of orphan
discovery.

---

## 6. Impact on listing

**None.** No key moves. The `S | … | 'O' |` range still contains object rows
and nothing else, scan boundaries are unchanged, `max-keys` accounting is
unchanged, and no new filtering is introduced. This is the main advantage over
§3.1 and §3.2.

---

## 7. Impact on sweeping

In **steady state, none**: the reaper removes markers within milliseconds of
the commit, so the sweeper's scan of the `P` prefix finds the same
near-empty range it finds today.

In the **degraded case** (reaper backlogged, or the process died with a
non-empty queue), the sweeper picks up the slack. Here the current sweeper
*is* weak, because it opens **one transaction per entry**
(`sweeper.cpp:110-121`). To stop that being the new bottleneck, batch it the
way `process_group_po_entry()` already batches group entries
(`sweeper.cpp:153-168`): issue all the `S:O` liveness `kv_async_get`s for a
scan window up front, then apply every resulting `kv_del` / `put(G:O)` in a
single transaction. The scan window is contiguous in the `P` namespace, so the
deletes are region-local for the same reason the reaper's are (§4.1).

That is a worthwhile change on its own merits and is listed as optional in
§8.

### 7.1 Pre-existing issue, noted in passing

`ref_tag` is monotonic per RGW, so the `P` namespace is append-only per RGW —
a classic TiKV region write hotspot, and the sequential-key problem that `D:`
keys explicitly avoid with `hash_prefix = Hash(ref_tag) % 32`
(`keys.cpp:73-81`, `data-tiering.md:60`). Phase 1 and the reaper both hit it.
Out of scope here, but the same `hash_prefix` trick applies, and the reaper's
batching would then produce 32 contiguous runs instead of one.

---

## 8. Implementation plan

Ordered, each step independently testable.

1. **`KvStore::clear(key)` / `clear_batch(keys)`** — `kv_store.hpp:235` has a
   non-transactional `set()` but no non-transactional delete. Add the
   symmetric call.
2. **Lease check.** Add `kPendingLeaseSec` / `kPendingLeaseSkewMarginSec` to
   `constants.hpp`, derived from `KVRGW_SWEEPER_MIN_AGE_SEC`
   (`kvrgw_runtime.cpp:130`) so the two can never be configured into an
   overlapping pair. Enforce it in `put_finalize()` where `f_po` is resolved
   (`service_impl.cpp:1976-1993`) — the `PoEntryView.gc.mtime` is already
   being parsed there.
3. **Reaper.** Small MPSC queue + drain thread, owned by `KvrgwRuntime`
   alongside the sweeper (`kvrgw_runtime.cpp:209-231`). Bounded; on overflow,
   drop the key (the sweeper is the backstop) and bump a counter.
4. **Single path.** Remove `service_impl.cpp:2037-2038`; on commit success in
   `put_object_phase3()` (`service_impl.cpp:1787-1790`), enqueue `ctx.po_key`.
   Note the early-return at `service_impl.cpp:1990` (`tr.kv_del(ctx.po_key)`
   on the already-committed path) also becomes an enqueue.
5. **Batch path.** Remove step 5 (`service_impl.cpp:848-852`); enqueue
   `ib.phase1_keys[]` on commit success (`service_impl.cpp:371-388`).
6. **Lease renewal during Phase 2** (§5.3) for objects whose upload exceeds
   `lease/2`.
7. *(optional, §7)* Batch the sweeper's single-`P:O` path into one transaction
   per scan window.
8. **Docs.** `PutObject-CHUNK_STORAGE-Protocol-Spec.md` §7.3 step 8 and §7.1,
   `Bucket-State-Management.md` §5 and §10, `transaction-safety.md`
   "PUT Phase 3". Note that §4.2 of the protocol spec currently documents the
   `P:O` key as containing `object_name`, which commit `a48122a8211` removed —
   worth fixing in the same pass.

### 8.1 Test plan

- Existing `FaultType::kAbortAfterSinglePhase2` (`service_impl.cpp:2256`)
  already covers "crash after Phase 2" → must still end in `G:O` + blob freed.
- New fault: "crash after Phase 3 commit, before reaper drain" → sweeper must
  delete the marker and must **not** create a `G:O`
  (`gc_value.cpp:217-223` path).
- New fault: force `now - mtime >= lease` → Phase 3 must abort before writing
  `O:`, and the blob must end up in `G:O`.
- Lease renewal: simulated Phase 2 longer than `lease`, verify the PUT
  succeeds and the marker is reaped.
- Region-count assertion is the actual goal: against a TiKV cluster with
  pre-split regions, count prewrite participants for a new-object PUT. Expect
  1. There is no FDB equivalent of this assertion — FDB does not expose
   participants — so it has to be an integration-level check.

---

## 9. Out of scope: overwrites

An overwrite calls `displace_old_object()`, which writes the old object's
`G:O` key into the `G` namespace, and under versioning may write a `V:` row.
Those are additional regions in the write set and this proposal does not
address them. After this change:

| PUT shape | Phase 3 regions (today → after) |
|---|---|
| new object, `CHUNK_STORAGE` | 2 → **1** |
| new object, batch of *N* in one bucket | 2 → **1** |
| overwrite, versioning disabled | 3 → 2 (`O:` + `G:O`) |
| overwrite, versioning enabled | 3 → 2 (`O:` + `V:`) |
| new object, `CHUNK_INLINE` | 1 → 1 (no `P:O` is ever written) |
| new object, `CHUNK_CHILD_D` | 2 → 2 — the `D:` chunk key is a second region (`service_impl.cpp:2113-2120`); a separate problem with a separate answer |

The `CHUNK_CHILD_D` row in that table is worth following up on: it is the same
shape of problem (one object row + one key in a far namespace) but the `D:`
key is *functional* data rather than a coordination marker, so it cannot be
deferred out of the commit. It needs §3.1's co-location treatment instead,
which is viable there because `D:` entries are not scanned for orphan
discovery.
