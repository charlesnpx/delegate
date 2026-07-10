#!/bin/sh
set -eu

# Source of truth stays in mise-en-place; delegate embeds a synchronized copy for builds.
mise_dir="${MISE_EN_PLACE_DIR:-"$HOME/WebstormProjects/mise-en-place"}"
src="$mise_dir/skills/delegate-contract/codex/SKILL.md"
script_dir=$(CDPATH= cd "$(dirname "$0")" && pwd)
repo_dir=$(CDPATH= cd "$script_dir/.." && pwd)
dst="$repo_dir/internal/policy/digest/delegate-contract.md"

if [ ! -f "$src" ]; then
	printf 'missing delegate-contract source: %s\n' "$src" >&2
	exit 1
fi

mkdir -p "$(dirname "$dst")"
cp "$src" "$dst"
