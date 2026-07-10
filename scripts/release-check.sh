#!/usr/bin/env bash
set -euo pipefail

ROOT=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd -P)
VERSION=$(tr -d '[:space:]' <"$ROOT/VERSION")
TAG=${1:-${RELEASE_TAG:-}}
HEAD_TAG=""

if [[ -z "$TAG" ]]; then
  TAG=$(git -C "$ROOT" describe --tags --exact-match 2>/dev/null || true)
else
  HEAD_TAG=$(git -C "$ROOT" describe --tags --exact-match 2>/dev/null || true)
  if [[ "$HEAD_TAG" != "$TAG" ]]; then
    if [[ -z "$HEAD_TAG" ]]; then
      printf 'release-check: current HEAD is not exactly requested tag %s\n' "$TAG" >&2
    else
      printf 'release-check: current HEAD is exactly tag %s, not requested tag %s\n' "$HEAD_TAG" "$TAG" >&2
    fi
    exit 1
  fi
fi

if [[ -z "$TAG" ]]; then
  printf 'release-check: no tag supplied and current commit is not exactly tagged\n' >&2
  printf 'usage: scripts/release-check.sh v%s\n' "$VERSION" >&2
  exit 2
fi

EXPECTED_TAG="v$VERSION"
if [[ "$TAG" != "$EXPECTED_TAG" ]]; then
  printf 'release-check: tag mismatch: got %s, want %s from VERSION\n' "$TAG" "$EXPECTED_TAG" >&2
  exit 1
fi

decode_installer_json() {
  local expected_operation=$1
  local raw
  raw=$(cat)

  if command -v python3 >/dev/null 2>&1; then
    printf '%s' "$raw" | EXPECTED_OPERATION=$expected_operation python3 -c '
import json
import os
import sys

raw = sys.stdin.read()

def fail(message):
    print(f"release-check: installer stdout failed JSON validation: {message}", file=sys.stderr)
    sys.exit(1)

try:
    doc = json.loads(raw)
except json.JSONDecodeError as exc:
    fail(f"not a single JSON document: {exc}")

if not isinstance(doc, dict):
    fail("top-level value is not an object")
expected_operation = os.environ["EXPECTED_OPERATION"]
if doc.get("schema") != 1:
    fail("schema=%r, want 1" % (doc.get("schema"),))
if doc.get("name") != "delegate":
    fail("name=%r, want delegate" % (doc.get("name"),))
if doc.get("kind") != "delegated":
    fail("kind=%r, want delegated" % (doc.get("kind"),))
if doc.get("operation") != expected_operation:
    fail("operation=%r, want %s" % (doc.get("operation"), expected_operation))

setup = doc.get("setup")
if not isinstance(setup, list):
    fail("setup is not an array")
executables = {entry.get("executable") for entry in setup if isinstance(entry, dict) and entry.get("kind") == "executable"}
if not {"go", "agentbus"}.issubset(executables):
    fail("setup must require go and agentbus")

targets = doc.get("targets")
if not isinstance(targets, dict):
    fail("targets is not an object")
for target, minimum_files in (("tools", 1), ("claude", 5), ("codex", 5)):
    target_doc = targets.get(target)
    if not isinstance(target_doc, dict):
        fail(f"targets.{target} is not an object")
    files = target_doc.get("files")
    if not isinstance(files, list) or len(files) < minimum_files:
        fail(f"targets.{target}.files has fewer than {minimum_files} entries")
    for index, file_doc in enumerate(files):
        if not isinstance(file_doc, dict):
            fail(f"targets.{target}.files[{index}] is not an object")
        sha256 = file_doc.get("sha256")
        if not isinstance(sha256, str) or not sha256:
            fail(f"targets.{target}.files[{index}].sha256 is missing")

version = doc.get("version")
if not isinstance(version, str) or not version:
    fail("version is missing")
print(version)
'
    return
  fi

  if command -v jq >/dev/null 2>&1; then
    local version
    if ! version=$(printf '%s' "$raw" | jq -er -s --arg operation "$expected_operation" '
      def fail($message): error("release-check: installer stdout failed JSON validation: " + $message);
      if length != 1 then fail("not a single JSON document") else .[0] end
      | if type != "object" then fail("top-level value is not an object") else . end
      | if .schema != 1 then fail("schema mismatch") else . end
      | if .name != "delegate" then fail("name mismatch") else . end
      | if .kind != "delegated" then fail("kind mismatch") else . end
      | if .operation != $operation then fail("operation mismatch") else . end
      | if (.setup | type) != "array" then fail("setup is not an array") else . end
      | if ([.setup[] | select(type == "object" and .kind == "executable") | .executable] | index("go") and index("agentbus")) then . else fail("setup must require go and agentbus") end
      | if (.targets | type) != "object" then fail("targets is not an object") else . end
      | reduce ["tools", "claude", "codex"][] as $target (.;
          if (.targets[$target] | type) != "object" then fail("targets." + $target + " is not an object")
          elif (.targets[$target].files | type) != "array" or (.targets[$target].files | length) < (if $target == "tools" then 1 else 5 end) then fail("targets." + $target + ".files is incomplete")
          elif any(.targets[$target].files[]; (type != "object") or (.sha256 | type) != "string" or .sha256 == "") then fail("targets." + $target + ".files has a missing sha256")
          else . end)
      | if (.version | type) != "string" or .version == "" then fail("version is missing") else .version end
    '); then
      return 1
    fi
    printf '%s\n' "$version"
    return
  fi

  printf 'release-check: python3 or jq is required to validate installer JSON output\n' >&2
  return 2
}

decode_cli_version() {
  local raw
  raw=$(cat)

  if command -v python3 >/dev/null 2>&1; then
    printf '%s' "$raw" | python3 -c '
import json
import sys

raw = sys.stdin.read()

def fail(message):
    print(f"release-check: CLI stdout failed JSON validation: {message}", file=sys.stderr)
    sys.exit(1)

try:
    doc = json.loads(raw)
except json.JSONDecodeError as exc:
    fail(f"not a single JSON document: {exc}")

version = doc.get("version") if isinstance(doc, dict) else None
if not isinstance(version, str) or not version:
    fail("version is missing")
print(version)
'
    return
  fi

  if command -v jq >/dev/null 2>&1; then
    local version
    if ! version=$(printf '%s' "$raw" | jq -er -s '
      def fail($message): error("release-check: CLI stdout failed JSON validation: " + $message);
      if length != 1 then fail("not a single JSON document") else .[0] end
      | if type != "object" then fail("top-level value is not an object") else . end
      | if (.version | type) != "string" or .version == "" then fail("version is missing") else .version end
    '); then
      return 1
    fi
    printf '%s\n' "$version"
    return
  fi

  printf 'release-check: python3 or jq is required to validate CLI JSON output\n' >&2
  return 2
}

STAGE=$(mktemp -d "${TMPDIR:-/tmp}/delegate-release-check.XXXXXX")
cleanup() {
  rm -rf -- "$STAGE"
}
trap cleanup EXIT

INSTALL_JSON=$(GOCACHE="${GOCACHE:-/private/tmp/delegate-gocache}" "$ROOT/install-skill.sh" --install --target all --json --install-root "$STAGE")
INSTALL_VERSION=$(printf '%s' "$INSTALL_JSON" | decode_installer_json "install")
if [[ "$INSTALL_VERSION" != "$VERSION" ]]; then
  printf 'release-check: installer version mismatch: got %s, want %s\n' "$INSTALL_VERSION" "$VERSION" >&2
  exit 1
fi

BIN="$STAGE/.local/bin/delegate"
if [[ ! -x "$BIN" ]]; then
  printf 'release-check: staged binary missing or not executable: %s\n' "$BIN" >&2
  exit 1
fi

CLI_JSON=$("$BIN" version --json)
CLI_VERSION=$(printf '%s' "$CLI_JSON" | decode_cli_version)
if [[ "$CLI_VERSION" != "$VERSION" ]]; then
  printf 'release-check: CLI version mismatch: got %s, want %s\n' "$CLI_VERSION" "$VERSION" >&2
  exit 1
fi

printf 'release-check: ok tag=%s version=%s\n' "$TAG" "$VERSION"
