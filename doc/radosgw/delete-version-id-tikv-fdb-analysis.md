# DELETE with version-id — TiKV / FDB isolation analysis

Cross-reference of the DELETE with version-id algorithm in
[Versioned-Bucket-Operations.md](Versioned-Bucket-Operations.md) against the
transaction isolation observations in
`~/Downloads/tikv-fdb-versioning-notes.md`.

---

## The stated serialization invariant vs. what Case 1 actually does

The doc claims:
> *Every operation that mutates the version chain writes to `:O:`. Write-Write conflict on `:O:` protects against concurrent modifications.*

**Case 1 falsifies this.** Deleting a non-current version reads `:O:` (to
establish it's Case 1) but never writes it. `:O:` is untouched. The
"serialization through `:O:`" story doesn't apply here.

---

## The real protection mechanism — and where it's fragile

The actual protection in the Case 1 vs. Case 2 race is not `:O:` but the
specific `:V:` key being promoted.

The race involves two clients targeting **different version IDs** concurrently:

- **Txn A**: `DELETE key?versionId=v3` — v3 is current → Case 2. The scan finds
  v2 as the version to promote.
- **Txn B**: `DELETE key?versionId=v2` — v2 is non-current → Case 1.

Both transactions happen to touch `:V:v2`, for different reasons:

| Transaction | Target version | Action on `:V:v2` | Write set (relevant keys) |
|---|---|---|---|
| Txn A — Case 2 | v3 (current) | delete from `:V:`, promote to `:O:` | `{:O:, :V:v2}` |
| Txn B — Case 1 | v2 (non-current) | delete from `:V:` | `{:V:v2}` |

Both write `:V:v2` — TiKV's write-write conflict fires, Txn A retries. On
retry it rescans `:V:`, finds v1, and promotes that instead. Correct.

But this is exactly the **dangling pointer scenario from §3** of the notes, and
the algorithm is safe only because of a non-obvious implementation detail: Case
2 **deletes** the promoted key from `:V:` (it moves it — delete from `:V:`,
write to `:O:`). If an implementation read v2 from `:V:` and wrote it to `:O:`
*without* deleting the `:V:` entry, the write sets would be disjoint (`{:O:}`
vs. `{:V:v2}`), TiKV would detect no conflict, and `:O:` would become a
dangling pointer to a version Case 1 had already deleted. No error surfaced.

This is the exact failure mode §3 warns about: correctness depends on a
semantic coupling ("the promoted key must appear in the write set") that nothing
in the API enforces structurally. A developer implementing Case 2 without that
understanding creates the bug silently.

---

## The range scan in Case 2 — TiKV gives no direct protection

Case 2 does:

```
range scan on ...V<object_name> with limit=1
```

Per §1 of the notes: *"A plain range scan gives you no protection against
concurrent writers modifying or inserting keys in that range."* TiKV has no
gap/predicate locking.

The algorithm's correctness here is indirect and requires checking three
sub-cases:

**Concurrent PUT:** A PUT writes both `:O:` (new current) and `:V:<old_version>`
(old current pushed down). Case 2 also writes `:O:`. Write-write conflict on
`:O:` catches this. ✓

**Concurrent Case 1 deleting the to-be-promoted key:** Covered by the `:V:v2`
write-set intersection analysis above. ✓ — but fragile; see above.

**Concurrent Case 1 deleting a *different* `:V:` key:** Say Case 2 is promoting
v2 and Case 1 concurrently deletes v1. Write sets `{:O:, :V:v2}` and `{:V:v1}`
are disjoint. Both commit. End state: `:O:` = v2, v1 is gone. This is correct —
v1 deletion is independent of the promotion of v2. ✓

The range scan is safe on TiKV *transitively* through the write set, but only
because the key topology of version_ids prevents a new `:V:` entry with a
"more recent" sort position from being inserted without also writing `:O:`
(which conflicts with Case 2). This reasoning requires holding several
invariants about the version numbering scheme simultaneously.

---

## FDB makes this substantially cleaner

On FDB (§6 of the notes), `getRange` automatically registers a
**read-conflict range** over the scanned keys. If anything is written into that
range by a committed transaction after Case 2's read version, Case 2 aborts at
commit time and retries.

This means:
- No need to reason about whether the specific `:V:` key being promoted appears
  in the write set.
- No need to reason about whether concurrent insertions into the range alias
  with the write set.
- The range scan itself is the serialization point, directly and by default.

The Case 2 algorithm is *correct by construction* on FDB even if implemented
naively (read `:V:`, write to `:O:`, don't bother deleting from `:V:`). On
TiKV, the same naive implementation introduces the dangling pointer race.

---

## The retry requirement (§2)

The notes flag that on conflict, the retrying transaction must **recompute from
scratch**, not re-submit cached values. Case 2 satisfies this structurally
because both reads (`:O:` and the `:V:` scan) are inside the transaction —
there's no pre-transaction snapshot being reused. However, the implementation
must actually re-execute the full transaction body on conflict, not just retry
the commit call. The algorithm description doesn't make this explicit.

---

## Summary of exposures

| Issue | Severity on TiKV | Severity on FDB |
|---|---|---|
| Stated "write `:O:`" invariant doesn't cover Case 1 | Misleading; actual protection is via `:V:` write-set intersection | Moot — read-conflict on scan handles it |
| Case 2 correctness depends on deleting the promoted `:V:` key | Silent dangling pointer if omitted | Not required for correctness |
| Range scan in Case 2 unprotected directly | True, but transitively safe given key topology | Not an issue — FDB registers read-conflict range automatically |
| No structural API enforcement (§3 recommendation) | A future operation that reads `:V:` without writing the used key silently breaks the scheme | Less critical due to read-conflict ranges |

The algorithm is correct as written under both TiKV and FDB, but the TiKV path
has a correctness argument that depends on a non-obvious structural property of
the "move" step. The §3 recommendation — locking the primary key
unconditionally via a shared helper, or bumping a generation counter — would
make TiKV safety as obvious as FDB's.

---

## DB-agnostic structural fix

### Does FDB have key locking?

No. FDB is fundamentally optimistic — there is no blocking/waiting primitive at
all. `AddWriteConflictRange` is the closest analog: it registers a
write-conflict on a key range without actually writing a value. But it only
causes a conflict when another transaction **reads** that range (read-conflict
vs. write-conflict is what FDB's Resolver checks). Two transactions that both
only call `AddWriteConflictRange` on the same key — without reading it — do not
conflict with each other. There is no server-side mutex to acquire.

The §3 pessimistic-lock approach is therefore TiKV-only. It cannot be ported to
FDB.

### The generation counter as a DB-agnostic invariant

The generation counter variant from §3 can be made DB-agnostic if every
object-mutating transaction both **reads and writes** the counter:

```
read  gen = Get(":G:objectname")     // registers read-conflict range on FDB
write Put(":G:objectname", gen + 1)  // puts key in write set on TiKV and FDB
```

Why both steps matter:

| | TiKV | FDB |
|---|---|---|
| Write alone | write-write conflict fires ✓ | only fires if the other txn reads the key — not guaranteed ✗ |
| Read + write | write-write conflict fires ✓ | read-conflict (auto-registered by read) vs. write-conflict (from write) fires ✓ |

The read is load-bearing on FDB. Without it, two concurrent transactions both
writing the key without reading it would not conflict — FDB's Resolver only
checks read-conflict vs. write-conflict, not write-write.

The value itself doesn't need to be a true counter — any read + write to a
shared key per object works. An incrementing integer is useful for debugging but
not required for the conflict semantics.

### Does this penalise FDB?

Minimally, and not structurally.

**What it costs:** one extra point read and one extra key write per transaction.
On FDB both can be pipelined with the `:O:` read already in the transaction —
`Get(":G:")` and `Get(":O:")` both return futures, the client sends them in one
batch, one round trip. The commit message grows by one mutation.

**What it doesn't cost:** no extra round trips on the uncontended path. FDB has
no lock acquisition RPC (unlike TiKV's `LockKeys`). The conflict check rides
inside the commit message you were sending anyway.

**Is it redundant on FDB?** Yes. FDB's automatic read-conflict range on the
`:V:` scan already provides serializability, including the Case 1 vs. Case 2
race. The generation counter adds no new protection on FDB — it exists solely to
make TiKV correct without requiring developers to reason per-operation about
write-set intersection.

The argument for keeping it anyway:

- It encodes the invariant structurally in a backend-agnostic way. The rule
  "every object mutation touches `:G:objectname`" is unconditional and
  mechanically checkable, whereas "every mutation must include its specific
  target `:V:` key in the write set" requires semantic reasoning about what each
  operation does.
- If a future operation reads from `:V:` to make a decision but doesn't happen
  to touch the promoted key, the generation counter catches the race on TiKV.
  FDB's read-conflict range would too, but relying on FDB to silently cover a
  gap that TiKV has is the kind of assumption that breaks when someone adds a
  new backend or changes the transaction API wrapper.

The FDB performance penalty is negligible. The value is making the TiKV safety
argument obvious and robust against future extensions.
