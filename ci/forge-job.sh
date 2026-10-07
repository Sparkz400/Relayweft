#!/usr/bin/env bash
# Copy alongside the pipeline as .relayweft-ci/forge-job.sh. Trusted branches only.
set -euo pipefail
umask 077

fail() { printf '%s\n' "$*" >&2; exit 1; }
case "${RW_FORGE:-}" in
  azure)
    : "${AZURE_DEVOPS_TOKEN:?set a secret Azure PAT}"
    : "${BUILD_REPOSITORY_URI:?missing checkout URL}"
    origin="$BUILD_REPOSITORY_URI"
    ;;
  bitbucket)
    : "${BITBUCKET_TOKEN:?set a secured Bitbucket token}"
    : "${BITBUCKET_REPO_FULL_NAME:?missing repository name}"
    origin="https://bitbucket.org/$BITBUCKET_REPO_FULL_NAME.git"
    ;;
  *) fail 'RW_FORGE must be azure or bitbucket' ;;
esac
: "${RW_BUDGET_TASK_USD:=5}"
[[ "$RW_BUDGET_TASK_USD" =~ ^[0-9]+([.][0-9]+)?$ ]] || fail 'invalid task budget'
node -e 'if (!(Number(process.argv[1]) > 0)) process.exit(1)' "$RW_BUDGET_TASK_USD" || fail 'CI requires a positive task budget'
[[ "${RW_LIMIT:=1}" =~ ^[1-9][0-9]*$ ]] || fail 'invalid issue limit'

tmp="$(mktemp -d)"
helper_key='credential.helper'
helper_value=""
cleanup() {
  # Remove only our helper, preserving preexisting configuration.
  if [ -n "$helper_value" ]; then git config --local --fixed-value --unset-all "$helper_key" "$helper_value" || true; fi
  rm -rf -- "$tmp"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

# Tokens stay in environment variables, never in URLs or .git/config. The
# helper checks the requested host and path before giving a credential.
export RW_CI_ORIGIN="$origin"
cat > "$tmp/credential.cjs" <<'JS'
const fs = require('fs');
const origin = new URL(process.env.RW_CI_ORIGIN);
if (origin.protocol !== 'https:' || origin.username || origin.password) process.exit(1);
if (process.argv[2] !== 'get') process.exit(0);
const fields = Object.fromEntries(fs.readFileSync(0, 'utf8').trim().split('\n').map(line => {
  const n = line.indexOf('='); return [line.slice(0,n), line.slice(n+1)];
}));
if (fields.protocol !== 'https' || fields.host !== origin.host ||
    fields.path !== origin.pathname.slice(1)) process.exit(0);
let username = 'oauth2', password = process.env.AZURE_DEVOPS_TOKEN;
if (process.env.RW_FORGE === 'bitbucket') {
  password = process.env.BITBUCKET_TOKEN || '';
  const colon = password.indexOf(':');
  username = colon < 0 ? 'x-token-auth' : password.slice(0, colon);
  if (colon >= 0) password = password.slice(colon+1);
}
if (password && !/[\r\n]/.test(password+username)) {
  console.log('username='+username); console.log('password='+password);
}
JS
node -e 'const u=new URL(process.env.RW_CI_ORIGIN); if(u.protocol!=="https:" || u.username || u.password) process.exit(1)' || fail 'checkout URL must be HTTPS without credentials'
git remote set-url origin "$origin"
# Pipelines must use a clean ephemeral checkout with no persisted checkout token.
if git config --local --get-regexp 'http\..*extraheader' >/dev/null; then
  fail 'checkout persisted an HTTP credential; disable persistCredentials before running this job'
fi
git config --local credential.useHttpPath true
helper_value="!node '$tmp/credential.cjs'"
git config --local --add "$helper_key" "$helper_value"
git config user.name 'Relayweft CI'
git config user.email 'relayweft-ci@users.noreply.github.com'

# The trusted template pins the binary digest independently of the download.
# Changing the version requires an independently reviewed RW_SHA256 as well.
if [ -z "${RW_BINARY:-}" ]; then
  case "$(uname -m)" in
    x86_64) arch=amd64; digest=5142e3b15fa70284593d61bbfa87b58a0fe5382de0560ef4ab792926d55735e9 ;;
    aarch64|arm64) arch=arm64; digest=06ec4f60b076fb1e07bbde21a128ee15f1b4044846db63bb0915da7570961276 ;;
    *) fail 'no pinned Linux binary for this architecture' ;;
  esac
  version="${RW_VERSION:-v0.4.0}"
  [[ "$version" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[A-Za-z0-9.-]+)?$ ]] || fail 'RW_VERSION must be an exact release tag'
  if [ "$version" != v0.4.0 ] && [ -z "${RW_SHA256:-}" ]; then fail 'a new version requires RW_SHA256'; fi
  digest="${RW_SHA256:-$digest}"
  [[ "$digest" =~ ^[a-fA-F0-9]{64}$ ]] || fail 'invalid RW_SHA256'
  curl --proto '=https' --tlsv1.2 -fsSL --retry 3 -o "$tmp/rw" "https://github.com/Sparkz400/Relayweft/releases/download/$version/rw-linux-$arch"
  printf '%s  %s\n' "$digest" "$tmp/rw" | sha256sum -c -
  chmod 700 "$tmp/rw"
  RW_BINARY="$tmp/rw"
fi
rw_bin="$(realpath "$RW_BINARY")"
[ -x "$rw_bin" ] || fail 'RW_BINARY must be an executable'
"$rw_bin" version

provider="${RW_PROVIDER:-}"
if [ -z "$provider" ]; then
  if [ -n "${CLAUDE_CODE_OAUTH_TOKEN:-}${ANTHROPIC_API_KEY:-}" ]; then provider=claude
  elif [ -n "${OPENAI_API_KEY:-}" ]; then provider=codex
  else fail 'set Claude credentials or OPENAI_API_KEY'; fi
fi
case "$provider" in
  claude)
    [ -n "${CLAUDE_CODE_OAUTH_TOKEN:-}${ANTHROPIC_API_KEY:-}" ] || fail 'missing Claude credentials'
    if [ "${RW_SKIP_AGENT_INSTALL:-false}" != true ]; then npm install -g --no-fund --no-audit "@anthropic-ai/claude-code@${CLAUDE_CODE_VERSION:-2.1.288}"; fi
    ;;
  codex)
    : "${OPENAI_API_KEY:?missing Codex API key}"
    if [ "${RW_SKIP_AGENT_INSTALL:-false}" != true ]; then
      npm install -g --no-fund --no-audit "@openai/codex@${CODEX_VERSION:-0.160.0}"
      printf '%s' "$OPENAI_API_KEY" | codex login --with-api-key
    fi
    unset OPENAI_API_KEY
    ;;
  *) fail 'RW_PROVIDER must be claude or codex' ;;
esac

out="$PWD/relayweft-report"
mkdir -p "$out"
printf '\n/relayweft-report/\n' >> "$(git rev-parse --git-path info/exclude)"
# This opt-in authorizes checked-in commands; only run on a reviewed branch.
if [ "${RW_TRUST_REPO_CONFIG:-false}" = true ] && [ -f .relayweft.yaml ]; then "$rw_bin" trust --yes; fi
args=(run --provider "$provider" --budget-task-usd "$RW_BUDGET_TASK_USD" --pr)
if [ -n "${RW_ISSUE:-}" ]; then args+=(--issue "$RW_ISSUE")
else args+=(--issues "label:${RW_ISSUES_LABEL:-rw}" --limit "$RW_LIMIT"); fi
if [ "${RW_DRAFT:-true}" = true ]; then args+=(--draft); fi
if [ -n "${RW_BASE:-}" ]; then args+=(--base "$RW_BASE"); fi
"$rw_bin" history --json -n 1000 > "$tmp/before.json"
set +e
"$rw_bin" "${args[@]}" 2>&1 | tee "$out/rw-run.log"
rc=${PIPESTATUS[0]}
set -e
if "$rw_bin" history --json -n 1000 > "$tmp/after.json"; then
  node - "$tmp/before.json" "$tmp/after.json" > "$tmp/ids" <<'JS'
const fs=require('fs');
const old=new Set(JSON.parse(fs.readFileSync(process.argv[2])).map(t=>t.id));
for(const t of JSON.parse(fs.readFileSync(process.argv[3]))) {
  if(!old.has(t.id) && /^[A-Za-z0-9_-]+$/.test(t.id)) console.log(t.id);
}
JS
  while IFS= read -r id; do
    "$rw_bin" report "$id" --out "$out/$id.html" || printf 'Could not render report %s\n' "$id" >&2
    "$rw_bin" report "$id" --md --out "$out/$id.md" || printf 'Could not render Markdown report %s\n' "$id" >&2
  done < "$tmp/ids"
else
  printf 'Could not read task history; see rw-run.log\n' >&2
fi
exit "$rc"
