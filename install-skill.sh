#!/usr/bin/env bash
set -euo pipefail

NAME="delegate"
SCHEMA="1"
OPERATION=""
TARGET="all"
INSTALL_ROOT_ARG=""
WARNINGS_JSON=""
CODEX_HOME_IGNORED_WARNING=0

die() {
  printf 'delegate installer: %s\n' "$*" >&2
  exit 2
}

script_dir() {
  local source=${BASH_SOURCE[0]}
  if [[ "$source" != */* ]]; then
    source=./$source
  fi
  local dir=${source%/*}
  (cd -- "$dir" && pwd -P)
}

REPO_ROOT=$(script_dir)
VERSION_FILE="$REPO_ROOT/VERSION"

if [[ ! -f "$VERSION_FILE" ]]; then
  die "VERSION file not found at $VERSION_FILE"
fi

VERSION=$(<"$VERSION_FILE")
VERSION=${VERSION//$'\r'/}
VERSION=${VERSION//$'\n'/}
VERSION=${VERSION//[[:space:]]/}
if [[ -z "$VERSION" ]]; then
  die "VERSION file is empty"
fi

json_escape() {
  local value=$1
  value=${value//\\/\\\\}
  value=${value//\"/\\\"}
  value=${value//$'\n'/\\n}
  value=${value//$'\r'/\\r}
  value=${value//$'\t'/\\t}
  printf '%s' "$value"
}

add_warning() {
  local escaped
  escaped=$(json_escape "$1")
  if [[ -n "$WARNINGS_JSON" ]]; then
    WARNINGS_JSON="$WARNINGS_JSON,"
  fi
  WARNINGS_JSON="$WARNINGS_JSON\"$escaped\""
}

set_operation() {
  local next=$1
  if [[ -n "$OPERATION" ]]; then
    die "exactly one operation flag is allowed"
  fi
  OPERATION=$next
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --plan)
      set_operation "plan"
      shift
      ;;
    --install)
      set_operation "install"
      shift
      ;;
    --uninstall)
      set_operation "uninstall"
      shift
      ;;
    --target)
      [[ $# -ge 2 ]] || die "--target requires claude, codex, tools, or all"
      TARGET=$2
      shift 2
      ;;
    --json)
      shift
      ;;
    --install-root)
      [[ $# -ge 2 ]] || die "--install-root requires an absolute path"
      INSTALL_ROOT_ARG=$2
      shift 2
      ;;
    -h|--help)
      printf 'Usage: %s [--plan|--install|--uninstall] --target claude|codex|tools|all [--json] [--install-root <abs>]\n' "$0" >&2
      exit 0
      ;;
    *)
      die "unknown flag: $1"
      ;;
  esac
done

if [[ -z "$OPERATION" ]]; then
  OPERATION="install"
fi

case "$TARGET" in
  claude|codex|tools|all) ;;
  *) die "--target must be claude, codex, tools, or all" ;;
esac

if [[ -n "$INSTALL_ROOT_ARG" && "$INSTALL_ROOT_ARG" != /* ]]; then
  die "--install-root must be absolute"
fi

canonical_existing_dir() {
  local dir=$1
  (cd -- "$dir" && pwd -P)
}

trim_trailing_slash() {
  local path=$1
  while [[ "$path" != "/" && "$path" == */ ]]; do
    path=${path%/}
  done
  printf '%s\n' "$path"
}

resolve_root() {
  local root
  if [[ -n "$INSTALL_ROOT_ARG" ]]; then
    root=$INSTALL_ROOT_ARG
    if [[ "$OPERATION" == "install" ]]; then
      mkdir -p -- "$root"
      canonical_existing_dir "$root"
      return
    fi
    if [[ -d "$root" ]]; then
      canonical_existing_dir "$root"
      return
    fi
    trim_trailing_slash "$root"
    return
  fi

  [[ -n "${HOME:-}" ]] || die "HOME is not set"
  [[ "$HOME" == /* ]] || die "HOME must be absolute"
  if [[ -d "$HOME" ]]; then
    canonical_existing_dir "$HOME"
    return
  fi
  trim_trailing_slash "$HOME"
}

ROOT=$(resolve_root)
TOOL_PATH="$ROOT/.local/bin/delegate"

tools_requested() {
  [[ "$TARGET" == "tools" || "$TARGET" == "all" ]]
}

claude_requested() {
  [[ "$TARGET" == "claude" || "$TARGET" == "all" ]]
}

codex_requested() {
  [[ "$TARGET" == "codex" || "$TARGET" == "all" ]]
}

path_inside_root() {
  local root=$1
  local path=$2
  [[ "$path" == "$root" || "$path" == "$root"/* ]]
}

decode_skill_dir() {
  local name=$1
  printf '%s\n' "${name//__colon__/:}"
}

claude_skills_root() {
  printf '%s\n' "$ROOT/.claude/skills"
}

codex_skills_root() {
  local codex_home=${CODEX_HOME:-}
  if [[ -n "$codex_home" ]]; then
    [[ "$codex_home" == /* ]] || die "CODEX_HOME must be absolute"
    codex_home=$(trim_trailing_slash "$codex_home")
    if [[ -n "$INSTALL_ROOT_ARG" ]]; then
      if path_inside_root "$ROOT" "$codex_home"; then
        printf '%s\n' "$codex_home/skills"
        return
      fi
      if [[ "$CODEX_HOME_IGNORED_WARNING" -eq 0 ]]; then
        CODEX_HOME_IGNORED_WARNING=1
        add_warning "CODEX_HOME is outside --install-root; staged codex skills use .codex under the install root"
      fi
    else
      printf '%s\n' "$codex_home/skills"
      return
    fi
  fi
  printf '%s\n' "$ROOT/.codex/skills"
}

set_skill_dirs_for_target() {
  local target=$1
  case "$target" in
    claude|codex)
      SKILL_DIRS=(delegate__colon__rescue__colon__claude delegate__colon__rescue__colon__codex delegate__colon__rescue__colon__cursor delegate__colon__review__colon__claude delegate__colon__review__colon__codex delegate__colon__review__colon__cursor delegate__colon__adversarial-review__colon__claude delegate__colon__adversarial-review__colon__codex delegate__colon__adversarial-review__colon__cursor delegate__colon__status delegate__colon__result delegate__colon__cancel delegate__colon__setup delegate__colon__config)
      ;;
    *)
      die "unsupported skill target $target"
      ;;
  esac
}

set_legacy_skill_dirs_for_target() {
  local target=$1
  case "$target" in
    claude)
      LEGACY_SKILL_DIRS=(codex__colon__rescue codex__colon__review codex__colon__adversarial-review codex__colon__status codex__colon__result codex__colon__cancel)
      ;;
    codex)
      LEGACY_SKILL_DIRS=(claude__colon__rescue claude__colon__review claude__colon__adversarial-review claude__colon__status claude__colon__result claude__colon__cancel)
      ;;
    *)
      die "unsupported skill target $target"
      ;;
  esac
}

skill_root_for_target() {
  case "$1" in
    claude) claude_skills_root ;;
    codex) codex_skills_root ;;
    *) die "unsupported skill target $1" ;;
  esac
}

skill_file_path() {
  local target=$1
  local escaped=$2
  local root decoded
  root=$(skill_root_for_target "$target")
  decoded=$(decode_skill_dir "$escaped")
  printf '%s\n' "$root/$decoded/SKILL.md"
}

sha256_file() {
  local path=$1
  if command -v shasum >/dev/null 2>&1; then
    shasum -a 256 "$path" | awk '{print $1}'
    return
  fi
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$path" | awk '{print $1}'
    return
  fi
  die "sha256 tool not found; install shasum or sha256sum"
}

build_delegate() {
  local output=$1
  local err_file
  err_file=$(mktemp "${TMPDIR:-/tmp}/delegate-build.XXXXXX")
  rm -f -- "$output"
  mkdir -p -- "$(dirname -- "$output")"

  if (
    cd -- "$REPO_ROOT"
    go build -mod=readonly -trimpath -ldflags "-X main.Version=$VERSION" -o "$output" ./cmd/delegate
  ) 2>"$err_file"; then
    rm -f -- "$err_file"
    return
  fi

  printf 'delegate installer: go build failed\n' >&2
  sed 's/^/go build: /' "$err_file" >&2
  rm -f -- "$err_file"
  exit 1
}

live_install_root() {
  [[ -n "${HOME:-}" && "$HOME" == /* && -d "$HOME" ]] || return 1
  canonical_existing_dir "$HOME"
}

record_codex_sandbox_action() {
  codex_requested || return 0

  local config_path agentbus_state agentbus_cache delegate_state codex_home state_home cache_home
  case "$OPERATION" in
    plan)
      if [[ -n "${HOME:-}" && "$HOME" == /* ]]; then
        codex_home=${CODEX_HOME:-"$HOME/.codex"}
        state_home=${XDG_STATE_HOME:-"$HOME/.local/state"}
        if [[ -n "${AGENTBUS_STATE_ROOT:-}" ]]; then
          agentbus_state=$AGENTBUS_STATE_ROOT
        else
          agentbus_state="$state_home/agentbus"
        fi
        if [[ "${OSTYPE:-}" == darwin* ]]; then
          agentbus_cache="$HOME/Library/Caches/agentbus"
        else
          cache_home=${XDG_CACHE_HOME:-"$HOME/.cache"}
          agentbus_cache="$cache_home/agentbus"
        fi
        if [[ "$codex_home" == /* && "$state_home" == /* && "$agentbus_state" == /* && "$agentbus_cache" == /* && ( -z "${XDG_CACHE_HOME:-}" || "$XDG_CACHE_HOME" == /* ) ]]; then
          config_path="$codex_home/config.toml"
          delegate_state="$state_home/delegate"
          add_warning "codex sandbox writable_roots would-configure: $agentbus_state, $agentbus_cache, $delegate_state (config $config_path)"
        else
          add_warning "codex sandbox writable_roots skipped: HOME, CODEX_HOME, AGENTBUS_STATE_ROOT, XDG_CACHE_HOME, and XDG_STATE_HOME must resolve to absolute paths"
        fi
      else
        add_warning "codex sandbox writable_roots skipped: HOME, CODEX_HOME, AGENTBUS_STATE_ROOT, XDG_CACHE_HOME, and XDG_STATE_HOME must resolve to absolute paths"
      fi
      ;;
    uninstall)
      add_warning "codex sandbox writable_roots entries left in place; uninstall does not remove security configuration automatically"
      ;;
    install)
      local live_root
      live_root=$(live_install_root) || {
        add_warning "codex sandbox writable_roots skipped: live HOME directory is unavailable"
        return 0
      }
      # mise-en-place invokes delegated installers with a temporary
      # --install-root, then copies staged files into the live destination.
      # Configuring from that staged invocation would mutate the user's Codex
      # sandbox even though the staged skills have not been installed live.
      # This expected skip is intentionally silent rather than a warning.
      if [[ "$ROOT" != "$live_root" ]]; then
        return 0
      fi

      local configure_bin temp_bin="" result
      if tools_requested; then
        configure_bin=$TOOL_PATH
      else
        if ! command -v go >/dev/null 2>&1; then
          add_warning "codex sandbox writable_roots skipped: go is required to run the Go TOML configurator"
          return 0
        fi
        temp_bin=$(mktemp "${TMPDIR:-/tmp}/delegate-codex-sandbox.XXXXXX")
        build_delegate "$temp_bin"
        configure_bin=$temp_bin
      fi
      if result=$("$configure_bin" configure-codex-sandbox 2>&1); then
        add_warning "$result"
      else
        add_warning "codex sandbox writable_roots skipped: $result"
      fi
      if [[ -n "$temp_bin" ]]; then
        rm -f -- "$temp_bin"
      fi
      ;;
  esac
}

install_skills_for_target() {
  local target=$1
  local escaped src dest
  set_skill_dirs_for_target "$target"
  for escaped in "${SKILL_DIRS[@]}"; do
    src="$REPO_ROOT/skills/$escaped/SKILL.md"
    [[ -f "$src" ]] || die "skill source not found: $src"
    dest=$(skill_file_path "$target" "$escaped")
    mkdir -p -- "$(dirname -- "$dest")"
    cp -- "$src" "$dest"
    chmod 0644 "$dest"
  done
}

uninstall_skills_for_target() {
  local target=$1
  local escaped path
  set_skill_dirs_for_target "$target"
  for escaped in "${SKILL_DIRS[@]}"; do
    path=$(skill_file_path "$target" "$escaped")
    rm -rf -- "$(dirname -- "$path")"
  done
}

remove_legacy_skills_for_target() {
  local target=$1
  local escaped path
  set_legacy_skill_dirs_for_target "$target"
  for escaped in "${LEGACY_SKILL_DIRS[@]}"; do
    path=$(skill_file_path "$target" "$escaped")
    rm -rf -- "$(dirname -- "$path")"
  done
}

TOOL_SHA=""

case "$OPERATION" in
  plan)
    ;;
  install)
    if tools_requested; then
      command -v go >/dev/null 2>&1 || die "go executable not found"
      build_delegate "$TOOL_PATH"
      TOOL_SHA=$(sha256_file "$TOOL_PATH")
      [[ -n "$TOOL_SHA" ]] || die "failed to compute sha256 for $TOOL_PATH"
    fi
    if claude_requested; then
      install_skills_for_target claude
      remove_legacy_skills_for_target claude
    fi
    if codex_requested; then
      install_skills_for_target codex
      remove_legacy_skills_for_target codex
    fi
    if ! command -v agentbus >/dev/null 2>&1; then
      add_warning "agentbus executable not found; install agentbus before using delegate skills"
    fi
    ;;
  uninstall)
    if tools_requested; then
      rm -f -- "$TOOL_PATH"
    fi
    if claude_requested; then
      uninstall_skills_for_target claude
      remove_legacy_skills_for_target claude
    fi
    if codex_requested; then
      uninstall_skills_for_target codex
      remove_legacy_skills_for_target codex
    fi
    ;;
  *)
    die "unsupported operation: $OPERATION"
    ;;
esac

record_codex_sandbox_action

print_warnings() {
  printf '[%s]' "$WARNINGS_JSON"
}

print_file() {
  local path=$1
  local sha=${2:-}
  printf '{"path":"%s"' "$(json_escape "$path")"
  if [[ -n "$sha" ]]; then
    printf ',"sha256":"%s"' "$(json_escape "$sha")"
  fi
  printf '}'
}

print_tool_target() {
  printf '"tools":{"files":['
  print_file "$TOOL_PATH" "$TOOL_SHA"
  printf ']}'
}

print_skill_target() {
  local target=$1
  local first=1
  local escaped path sha
  set_skill_dirs_for_target "$target"
  printf '"%s":{"files":[' "$(json_escape "$target")"
  for escaped in "${SKILL_DIRS[@]}"; do
    path=$(skill_file_path "$target" "$escaped")
    sha=""
    if [[ "$OPERATION" == "install" && -f "$path" ]]; then
      sha=$(sha256_file "$path")
    fi
    if [[ $first -eq 0 ]]; then
      printf ','
    fi
    first=0
    print_file "$path" "$sha"
  done
  printf '],"removed":['
  first=1
  set_legacy_skill_dirs_for_target "$target"
  for escaped in "${LEGACY_SKILL_DIRS[@]}"; do
    path=$(skill_file_path "$target" "$escaped")
    if [[ $first -eq 0 ]]; then
      printf ','
    fi
    first=0
    printf '{"path":"%s"}' "$(json_escape "$path")"
  done
  printf ']}'
}

print_targets() {
  local names=()
  local first=1
  local name
  case "$TARGET" in
    all) names=(tools claude codex) ;;
    *) names=("$TARGET") ;;
  esac
  printf '{'
  for name in "${names[@]}"; do
    if [[ $first -eq 0 ]]; then
      printf ','
    fi
    first=0
    case "$name" in
      tools) print_tool_target ;;
      claude|codex) print_skill_target "$name" ;;
      *) die "unsupported target $name" ;;
    esac
  done
  printf '}'
}

printf '{"schema":%s,"name":"%s","version":"%s","operation":"%s","kind":"delegated"' \
  "$SCHEMA" "$(json_escape "$NAME")" "$(json_escape "$VERSION")" "$(json_escape "$OPERATION")"

printf ',"setup":[{"kind":"executable","executable":"go","remediation":"Install Go and ensure go is on PATH before installing delegate tools."},{"kind":"executable","executable":"agentbus","remediation":"Install agentbus first; delegate skills call agentbus through the delegate CLI."}]'

printf ',"targets":'
print_targets
printf ',"warnings":'
print_warnings
printf '}\n'
