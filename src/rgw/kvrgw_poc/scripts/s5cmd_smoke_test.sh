#!/bin/bash
# S3 smoke test for the kv-rgw gateway using s5cmd:
# mb, cp (upload), cp (download) + verify, head, ls, rm, rb.
#
#   ./s5cmd_smoke_test.sh [endpoint-url]      (default: http://127.0.0.1:9080)
#
# Environment: AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY (default test/test),
# AWS_REGION (default us-east-1), NUM_OBJECTS (default 5), KEEP=1 to leave the
# bucket in place on failure. Exits non-zero on the first failed step.
set -euo pipefail

ENDPOINT="${1:-${S3_ENDPOINT_URL:-http://127.0.0.1:9080}}"
export AWS_ACCESS_KEY_ID="${AWS_ACCESS_KEY_ID:-test}"
export AWS_SECRET_ACCESS_KEY="${AWS_SECRET_ACCESS_KEY:-test}"
export AWS_REGION="${AWS_REGION:-us-east-1}"
NUM_OBJECTS="${NUM_OBJECTS:-5}"

BUCKET="smoke-$(date +%s)-$$"
WORK="$(mktemp -d)"
S5=(s5cmd --endpoint-url "$ENDPOINT")
created=0

cleanup() {
  local rc=$?
  if [[ $rc -ne 0 && "${KEEP:-0}" != 1 && $created -eq 1 ]]; then
    "${S5[@]}" rm "s3://$BUCKET/*" >/dev/null 2>&1 || true
    "${S5[@]}" rb "s3://$BUCKET" >/dev/null 2>&1 || true
  fi
  rm -rf "$WORK"
  if [[ $rc -eq 0 ]]; then echo "PASS"; else echo "FAIL (exit $rc)"; fi
}
trap cleanup EXIT

step() { echo "== $*"; }
fail() { echo "FAILED: $*" >&2; exit 1; }

command -v s5cmd >/dev/null || fail "s5cmd not found in PATH"

# Sizes span the gateway's storage tiers (inline, KV store, file data).
sizes=(10 200 3000 100000 2000000)
keys=()
for ((i = 0; i < NUM_OBJECTS; i++)); do
  keys+=("obj-$i")
  head -c "${sizes[$((i % ${#sizes[@]}))]}" /dev/urandom > "$WORK/obj-$i"
done

step "create bucket s3://$BUCKET at $ENDPOINT"
"${S5[@]}" mb "s3://$BUCKET"
created=1

step "upload ${#keys[@]} objects"
for k in "${keys[@]}"; do
  "${S5[@]}" cp "$WORK/$k" "s3://$BUCKET/$k"
done

step "get objects and compare content"
mkdir "$WORK/dl"
for k in "${keys[@]}"; do
  "${S5[@]}" cp "s3://$BUCKET/$k" "$WORK/dl/$k"
  cmp "$WORK/$k" "$WORK/dl/$k" || fail "content mismatch for $k"
done

step "head objects and check sizes"
for k in "${keys[@]}"; do
  out="$("${S5[@]}" --json head "s3://$BUCKET/$k")"
  want="$(stat -c %s "$WORK/$k")"
  grep -Eq "\"[A-Za-z_]*([Ll]ength|size)\"[: ]+$want\b" <<<"$out" \
    || fail "head $k: expected size $want, got: $out"
done

step "list objects"
listing="$("${S5[@]}" ls "s3://$BUCKET/")"
for k in "${keys[@]}"; do
  grep -q " $k\$" <<<"$listing" || fail "$k missing from listing: $listing"
done
count="$(grep -c . <<<"$listing")"
[[ "$count" -eq ${#keys[@]} ]] || fail "expected ${#keys[@]} listed objects, got $count"

step "delete objects"
for k in "${keys[@]}"; do
  "${S5[@]}" rm "s3://$BUCKET/$k"
done
[[ -z "$("${S5[@]}" ls "s3://$BUCKET/" 2>/dev/null || true)" ]] \
  || fail "bucket not empty after deleting objects"

step "delete bucket"
"${S5[@]}" rb "s3://$BUCKET"
created=0
if "${S5[@]}" ls "s3://$BUCKET/" >/dev/null 2>&1; then
  fail "bucket still exists after rb"
fi
