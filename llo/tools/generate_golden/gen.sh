#!/bin/bash -ex

# This script generates golden files for LLOOutcomeProtoV1 serialization comparison tests.
# It checks out the specified tag, builds the code, runs the generation program, and saves the golden files.
# After completion, it returns to the original branch.
set -e

TAG="$1"
if [ -z "$TAG" ]; then
  echo "Usage: $0 <tag>"
  exit 1
fi

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
LLO_DIR="$REPO_ROOT/llo"

# Get current branch/tag
CURRENT_REF=$(git rev-parse --abbrev-ref HEAD 2>/dev/null || git describe --tags --exact-match HEAD 2>/dev/null || echo "HEAD")

echo "Current git reference: $CURRENT_REF"
echo "Generating golden files from $TAG..."

cd "$REPO_ROOT"
git checkout "$TAG"
cd "$LLO_DIR"
cd tools/generate_golden
go run main.go "$LLO_DIR"
cd "$LLO_DIR"
cd "$REPO_ROOT"
git checkout "$CURRENT_REF"

echo "Golden files generated successfully in llo/testdata/outcome_serialization/"