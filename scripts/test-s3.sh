#!/bin/sh
set -eu

cd "$(dirname "$0")/.."

if [ -z "${PAGE_TEST_S3_ENDPOINT:-}" ]; then
  docker compose -f compose.yaml up -d s3
  PAGE_TEST_S3_ENDPOINT="http://127.0.0.1:${PAGE_S3_PORT:-19090}"
  attempts=0
  until curl --fail --silent "$PAGE_TEST_S3_ENDPOINT/favicon.ico" >/dev/null; do
    attempts=$((attempts + 1))
    if [ "$attempts" -ge 60 ]; then
      docker compose -f compose.yaml logs --tail 30 s3
      echo "S3 API did not become ready; set PAGE_TEST_S3_ENDPOINT for a remote Docker daemon." >&2
      exit 1
    fi
    sleep 1
  done
fi

export PAGE_TEST_S3_ENDPOINT
export PAGE_TEST_S3_BUCKET="${PAGE_TEST_S3_BUCKET:-page-storage-test}"
export AWS_REGION="${AWS_REGION:-us-east-1}"
export AWS_ACCESS_KEY_ID="${AWS_ACCESS_KEY_ID:-page-dev}"
export AWS_SECRET_ACCESS_KEY="${AWS_SECRET_ACCESS_KEY:-page-dev-secret}"
export AWS_EC2_METADATA_DISABLED=true

go test -count=1 -timeout 2m -run '^(TestS3Integration|TestMVPWithS3|TestExpansionWithS3)$' -v ./internal/storage ./internal/app
