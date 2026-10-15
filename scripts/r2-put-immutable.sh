#!/usr/bin/env bash
# Uploads one file to a versioned R2 key that must never change once written.
#
#   r2-put-immutable.sh <file> <key> <content-type>
#
# Needs R2_BUCKET and ENDPOINT in the environment and a configured aws CLI.
#
# A published update feed names these exact bytes, so a key that already holds
# them is left alone, and a key that holds different bytes fails the release:
# overwriting it would break every client holding the earlier feed. The sha256
# is stored as object metadata on upload; an object uploaded before that is
# downloaded and hashed instead.
set -euo pipefail

file=$1
key=$2
type=$3

sha256() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | cut -d' ' -f1
  else
    shasum -a 256 "$1" | cut -d' ' -f1
  fi
}

want=$(sha256 "$file")
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

if existing=$(aws s3api head-object --bucket "$R2_BUCKET" --key "$key" \
    --endpoint-url "$ENDPOINT" --query 'Metadata.sha256' --output text 2>"$tmp/head.err"); then
  if [ -z "$existing" ] || [ "$existing" = "None" ]; then
    aws s3 cp "s3://${R2_BUCKET}/${key}" "$tmp/existing" --endpoint-url "$ENDPOINT" >/dev/null
    existing=$(sha256 "$tmp/existing")
  fi
  if [ "$existing" = "$want" ]; then
    echo "$key already holds these bytes (sha256 $want); not uploading it again"
    exit 0
  fi
  echo "::error title=Versioned artifact already published::$key already holds different bytes (sha256 $existing, this build $want). A published feed may name the old bytes, so it is not overwritten. A rebuilt version needs a new version number."
  exit 1
fi
if ! grep -qE '\(404\)|Not Found' "$tmp/head.err"; then
  cat "$tmp/head.err" >&2
  echo "::error::Could not check whether $key already exists"
  exit 1
fi

echo "Uploading $key (sha256 $want)..."
aws s3 cp "$file" "s3://${R2_BUCKET}/${key}" \
  --endpoint-url "$ENDPOINT" \
  --content-type "$type" \
  --cache-control "public, max-age=31536000, immutable" \
  --metadata "sha256=$want"
aws s3 cp "s3://${R2_BUCKET}/${key}" "$tmp/readback" --endpoint-url "$ENDPOINT" >/dev/null
cmp "$file" "$tmp/readback"
