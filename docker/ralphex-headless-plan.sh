#!/usr/bin/env bash
set -euo pipefail

usage() {
    cat >&2 <<'EOF'
usage: ralphex-headless-plan [--profile-dir <dir> | --profile <codex|codex-planning|pi|pi-planning>] "plan request"

Generates a ralphex-compatible plan file non-interactively via codex, writes it
under docs/plans/, and exits. It does not start ralphex execution.

Examples:
  ralphex-headless-plan --profile codex-planning "Implement Hacker News collector"
  ralphex-headless-plan --profile pi-planning "Implement Hacker News collector"
  ralphex-headless-plan --profile-dir /opt/ralphex-profiles/codex-planning "Implement Hacker News collector"
  ralphex-headless-plan "Implement Hacker News collector"
EOF
    exit 2
}

trim() {
    local s="$1"
    s="${s#"${s%%[![:space:]]*}"}"
    s="${s%"${s##*[![:space:]]}"}"
    printf '%s' "$s"
}

read_cfg() {
    local key="$1" default="${2:-}" config_file="$3"
    if [ ! -f "$config_file" ]; then
        printf '%s' "$default"
        return
    fi

    local line value
    line="$(awk -F= -v wanted="$key" '
        $0 ~ "^[[:space:]]*" wanted "[[:space:]]*=" {
            sub(/^[[:space:]]+/, "", $2)
            print $2
            exit
        }
    ' "$config_file")"

    if [ -z "$line" ]; then
        printf '%s' "$default"
        return
    fi

    value="${line%%#*}"
    value="$(trim "$value")"
    value="${value%\"}"
    value="${value#\"}"
    printf '%s' "${value:-$default}"
}

slugify() {
    printf '%s' "$1" \
        | tr '[:upper:]' '[:lower:]' \
        | sed -E 's/[^a-z0-9]+/-/g; s/^-+//; s/-+$//; s/-{2,}/-/g' \
        | cut -c1-64
}

split_words() {
    local raw="$1"
    if [ -z "$(trim "$raw")" ]; then
        return 0
    fi
    # shellcheck disable=SC2206
    local words=( $raw )
    printf '%s\n' "${words[@]}"
}

ensure_repo_root() {
    if ! git_root="$(git rev-parse --show-toplevel 2>/dev/null)"; then
        echo "error: ralphex-headless-plan must run inside a git repository" >&2
        exit 2
    fi
    cd "$git_root"
    printf '%s' "$git_root"
}

split_args() {
    prompt=""
    profile_name=""
    profile_dir_override=""

    if [ "$#" -eq 0 ]; then
        usage
    fi

    while [ "$#" -gt 0 ]; do
        case "$1" in
            --profile)
                if [ "$#" -lt 2 ]; then
                    echo "error: --profile requires a value" >&2
                    usage
                fi
                profile_name="$2"
                shift 2
                ;;
            --profile-dir)
                if [ "$#" -lt 2 ]; then
                    echo "error: --profile-dir requires a value" >&2
                    usage
                fi
                profile_dir_override="$2"
                shift 2
                ;;
            --)
                usage
                ;;
            -*)
                echo "error: unexpected argument: $1" >&2
                usage
                ;;
            *)
                if [ -n "$prompt" ]; then
                    echo "error: unexpected extra argument: $1" >&2
                    usage
                fi
                prompt="$1"
                shift
                ;;
        esac
    done

    if [ -z "$(trim "$prompt")" ]; then
        echo "error: empty plan request" >&2
        usage
    fi
}

split_args "$@"
repo_root="$(ensure_repo_root)"

if [ -n "$profile_name" ] && [ -n "$profile_dir_override" ]; then
    echo "error: use either --profile or --profile-dir, not both" >&2
    exit 2
fi

if [ -n "$profile_dir_override" ]; then
    config_dir="$profile_dir_override"
elif [ -n "$profile_name" ]; then
    case "$profile_name" in
        codex|codex-planning|pi|pi-planning)
            config_dir="/opt/ralphex-profiles/$profile_name"
            ;;
        *)
            echo "error: unsupported profile '$profile_name'; use codex, codex-planning, pi, pi-planning, or pass --profile-dir" >&2
            exit 2
            ;;
    esac
else
    config_dir="${RALPHEX_CONFIG_DIR:-$HOME/.config/ralphex}"
fi

config_file="$config_dir/config"
if [ ! -f "$config_file" ]; then
    echo "error: profile config not found: $config_file" >&2
    exit 2
fi

executor="$(read_cfg executor "" "$config_file")"
claude_command="$(read_cfg claude_command "" "$config_file")"
claude_args_raw="$(read_cfg claude_args "" "$config_file")"

if [ "$executor" != "codex" ] && [ -z "$claude_command" ]; then
    echo "error: unsupported profile config: expected executor=codex or claude_command in $config_file" >&2
    exit 2
fi

codex_model="$(read_cfg codex_model "gpt-5.6-terra" "$config_file")"
codex_effort="$(read_cfg codex_reasoning_effort "medium" "$config_file")"
codex_sandbox="$(read_cfg codex_sandbox "workspace-write" "$config_file")"
task_model="$(read_cfg task_model "" "$config_file")"

plan_dir="$repo_root/docs/plans"
mkdir -p "$plan_dir"

today="$(date +%Y%m%d)"
base_slug="$(slugify "$prompt")"
if [ -z "$base_slug" ]; then
    base_slug="plan"
fi
plan_file="$plan_dir/${today}-${base_slug}.md"
suffix=2
while [ -e "$plan_file" ]; do
    plan_file="$plan_dir/${today}-${base_slug}-${suffix}.md"
    suffix=$((suffix + 1))
done

tmp_output="$(mktemp)"
tmp_prompt="$(mktemp)"
cleanup() {
    rm -f "$tmp_output" "$tmp_prompt"
}
trap cleanup EXIT

cat >"$tmp_prompt" <<EOF
You are creating an implementation plan for ralphex.

User request:
$prompt

Repository requirements:
- Inspect the current repository before writing the plan.
- If there are existing plans under docs/plans/, use them as style references only.
- Output a single complete markdown document only. Do not wrap it in code fences.
- Write the plan file in ralphex format so it can be executed directly by ralphex.
- Include a concise title line starting with "# Plan:".
- Include a "## Validation Commands" section with concrete commands relevant to this repo.
- Include one or more task sections formatted exactly as "### Task N: ...".
- Under each task, use unchecked markdown checkboxes ("- [ ] ...").
- Be specific enough for autonomous execution and review.
- Do not include commentary outside the plan document.
EOF

if [ "$executor" = "codex" ]; then
    echo "ralphex-headless-plan: generating plan via codex (${codex_model}, effort=${codex_effort}, sandbox=${codex_sandbox}, profile=${config_dir})" >&2
    codex exec \
        --cd "$repo_root" \
        --sandbox "$codex_sandbox" \
        --model "$codex_model" \
        -c "model_reasoning_effort=\"$codex_effort\"" \
        -o "$tmp_output" \
        - <"$tmp_prompt"
else
    if [ ! -x "$claude_command" ]; then
        echo "error: claude_command is not executable: $claude_command" >&2
        exit 2
    fi
    if [ -z "$task_model" ]; then
        echo "error: profile config is missing task_model: $config_file" >&2
        exit 2
    fi

    claude_args=()
    while IFS= read -r arg; do
        [ -n "$arg" ] && claude_args+=("$arg")
    done < <(split_words "$claude_args_raw")

    echo "ralphex-headless-plan: generating plan via claude_command (${claude_command}, model=${task_model}, profile=${config_dir})" >&2
    (
        cd "$repo_root"
        "$claude_command" "${claude_args[@]}" --model "$task_model" <"$tmp_prompt"
    ) | jq -r '
        select(.type == "content_block_delta")
        | .delta.text // empty
    ' >"$tmp_output"
fi

if [ ! -s "$tmp_output" ]; then
    echo "error: codex produced no plan output" >&2
    exit 1
fi

awk '
    BEGIN { started = 0 }
    /^```/ { next }
    {
        if (!started && $0 ~ /^# Plan:/) {
            started = 1
        }
        if (started) {
            print
        }
    }
' "$tmp_output" >"$plan_file"

if ! grep -q '^# Plan:' "$plan_file"; then
    echo "error: generated output is missing '# Plan:' header" >&2
    exit 1
fi
if ! grep -q '^## Validation Commands' "$plan_file"; then
    echo "error: generated output is missing '## Validation Commands' section" >&2
    exit 1
fi
if ! grep -q '^### Task [0-9]\+:' "$plan_file"; then
    echo "error: generated output is missing any '### Task N:' sections" >&2
    exit 1
fi

echo "ralphex-headless-plan: wrote $plan_file" >&2
printf '%s\n' "$plan_file"
