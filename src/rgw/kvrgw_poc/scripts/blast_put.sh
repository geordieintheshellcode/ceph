#!/usr/bin/env bash
# Blast PUTs of identical bytes under unique key names into rgwkv.
# Usage: ./blast_put.sh [count] [workers] [size_bytes]
set -euo pipefail

EP="${EP:-http://127.0.0.1:9080}"
BUCKET="${BUCKET:-load}"
COUNT="${1:-100000}"
WORKERS="${2:-512}"
SIZE="${3:-4000}"
RUN="$(date +%s)-$$"   # unique prefix per run, so reruns never overwrite

export AWS_ACCESS_KEY_ID="${AWS_ACCESS_KEY_ID:-test}"
export AWS_SECRET_ACCESS_KEY="${AWS_SECRET_ACCESS_KEY:-test}"
export AWS_REGION="${AWS_REGION:-us-east-1}"

S5=(s5cmd --endpoint-url "$EP" --numworkers "$WORKERS")

SRC="$(mktemp)"
trap 'rm -f "$SRC"' EXIT
head -c "$SIZE" /dev/urandom > "$SRC"

"${S5[@]}" mb "s3://$BUCKET" 2>/dev/null || true

echo "PUT $COUNT x ${SIZE}B to $EP/$BUCKET/$RUN/ with $WORKERS workers"
start=$(date +%s.%N)
seq 1 "$COUNT" | awk -v src="$SRC" -v b="$BUCKET" -v r="$RUN" \
  '{printf "cp %s s3://%s/%s/obj-%d\n", src, b, r, $1}' | "${S5[@]}" --stat run
end=$(date +%s.%N)

awk -v n="$COUNT" -v s="$start" -v e="$end" \
  'BEGIN{d=e-s; printf "done in %.2fs = %.0f PUT/s\n", d, n/d}'
