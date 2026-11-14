#!/bin/sh
# Isolated end-to-end smoke for the noc Go executable.
#
# Exercises server lifecycle, socket/file permissions, hook ingestion,
# projection, previews, malformed-input handling, rename, `server restart`
# (its own exit status, a new PID, flags honoured, identity kept), identity
# across a separate stop/start cycle, daemon-down spooling, session end,
# D-047 dropping, and event privacy. Every lifecycle command's exit status
# is asserted; a failed command is never silently retried another way.
#
# Safety: every path lives under a fresh mktemp directory; provider
# executables point at a nonexistent path AND every server start/restart
# passes --discovery=false, so no real Claude/Codex command can run and the
# user's real registry is never opened. Nothing outside the temp dir is
# written.
#
# Usage: docs/spec/verify/isolated-smoke.sh [path/to/noc]
#   default binary: go/bin/noc (run `make build` first)

set -u

here=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd -P)
root=$(CDPATH= cd -- "$here/../../.." && pwd -P)
M=${1:-$root/go/bin/noc}
[ -x "$M" ] || { echo "noc binary not found at $M (run make build)" >&2; exit 2; }
command -v python3 >/dev/null || { echo "python3 is required" >&2; exit 2; }

# Short base path: macOS limits Unix socket paths to 104 bytes.
D=$(mktemp -d /tmp/noc-smoke.XXXXXX)
export NOC_STATE_DIR="$D/state" NOC_RUNTIME_DIR="$D/run"
export NOC_CLAUDE_BIN="$D/no-such-claude" NOC_CODEX_BIN="$D/no-such-codex"
export NOC_CODEX_SOCKET="$D/no-such-codex.sock"
unset NOC_SOCKET NOC_DATABASE ZELLIJ_SESSION_NAME ZELLIJ_PANE_ID

pass=0 fail=0
ok()   { pass=$((pass + 1)); echo "ok   - $1"; }
bad()  { fail=$((fail + 1)); echo "FAIL - $1"; }
check() { if eval "$2"; then ok "$1"; else bad "$1"; fi; }
json() { python3 -c "import json,sys; d=json.load(sys.stdin); print($1)"; }
ingest() { # provider json -> prints captured stdout+stderr, returns exit code
    out=$(printf '%s' "$2" | "$M" hook ingest --provider "$1" 2>&1); rc=$?
    printf '%s' "$out"; return $rc
}
wait_for() { # python-expr-over-ls-json [all]
    flag=""; [ "${2:-}" = all ] && flag=--all
    i=0; while [ $i -lt 30 ]; do
        if [ "$("$M" ls --json $flag 2>/dev/null | json "$1" 2>/dev/null)" = True ]; then return 0; fi
        i=$((i + 1)); sleep 0.2
    done; return 1
}
start() { "$M" server --bg --discovery=false >/dev/null 2>&1; }
server_pid() { python3 -c "import json,sys; print(json.load(open(sys.argv[1]))['pid'])" "$D/run/noc.pid" 2>/dev/null; }
cleanup() { "$M" server stop >/dev/null 2>&1; echo "temp dir left for inspection: $D"; }
trap cleanup EXIT

C1=11111111-2222-4333-8444-555555555555   # Claude session, will be prompted + named
X1=22222222-2222-4333-8444-555555555555   # Codex session, ingested while server down
X2=33333333-2222-4333-8444-555555555555   # Codex session, never prompted, then ended
PROMPT='Please   summarize

 the   repo'

echo "# lifecycle"
start; rc=$?
check "server --bg exits 0 and creates the socket (LIFE)" '[ $rc -eq 0 ] && [ -S "$D/run/noc.sock" ]'
check "socket is 0600 (INV-010)"   '[ "$(stat -f %Sp "$D/run/noc.sock" 2>/dev/null || stat -c %A "$D/run/noc.sock")" = srw------- ]'
check "pid file is 0600"           '[ "$(stat -f %Sp "$D/run/noc.pid" 2>/dev/null || stat -c %A "$D/run/noc.pid")" = -rw------- ]'
check "database is 0600"           '[ "$(stat -f %Sp "$D/state/noc.db" 2>/dev/null || stat -c %A "$D/state/noc.db")" = -rw------- ]'
check "runtime dir is 0700"        '[ "$(stat -f %Sp "$D/run" 2>/dev/null || stat -c %A "$D/run")" = drwx------ ]'
check "state dir is 0700"          '[ "$(stat -f %Sp "$D/state" 2>/dev/null || stat -c %A "$D/state")" = drwx------ ]'
dup=$("$M" server --bg --discovery=false 2>&1); rc=$?
check "second server rejected with exit 1" '[ $rc -eq 1 ] && echo "$dup" | grep -q "already running"'
st=$("$M" server status --json 2>/dev/null)
check "status --json parses" 'echo "$st" | python3 -c "import json,sys; json.load(sys.stdin)"'

echo "# hook ingestion and projection"
out=$(ingest claude "{\"hook_event_name\":\"SessionStart\",\"session_id\":\"$C1\",\"cwd\":\"/tmp\",\"source\":\"startup\",\"model\":\"test-model\"}"); rc=$?
check "hook ingest exits 0 and is silent (INV-002)" '[ $rc -eq 0 ] && [ -z "$out" ]'
check "SessionStart creates a live hook row in review" \
  "wait_for 'any(a[\"native_session_id\"]==\"$C1\" and a[\"origin\"]==\"hook\" and a[\"live_state\"]==\"live\" and a[\"bucket\"]==\"review\" and a[\"display_state\"]==\"ready\" for a in d[\"agents\"])'"
check "model captured with hook provenance" \
  "wait_for 'any(a.get(\"model\")==\"test-model\" and a.get(\"model_source\")==\"hook\" for a in d[\"agents\"])'"
ID=$("$M" ls --json | json "[a['id'] for a in d['agents'] if a['native_session_id']=='$C1'][0]")
check "show by full id works" '"$M" show --json "$ID" >/dev/null 2>&1'

p=$(python3 -c "import json,sys; print(json.dumps({'hook_event_name':'UserPromptSubmit','session_id':'$C1','cwd':'/tmp','prompt':sys.argv[1]}))" "$PROMPT")
ingest claude "$p" >/dev/null
check "prompt moves agent to working" \
  "wait_for 'any(a[\"id\"]==\"$ID\" and a[\"bucket\"]==\"working\" for a in d[\"agents\"])'"
ir=$("$M" show --json "$ID" | json "d.get('initial_request')")
check "initial request is whitespace-compacted" 'echo "$ir" | grep -q "Please summarize the repo"'
check "initial request labelled first-observed" 'echo "$ir" | grep -q hook_first_observed_user_prompt'

long=$(python3 -c "print('y'*1500)")
ingest claude "{\"hook_event_name\":\"Stop\",\"session_id\":\"$C1\",\"cwd\":\"/tmp\",\"last_assistant_message\":\"$long\"}" >/dev/null
check "Stop returns agent to review" \
  "wait_for 'any(a[\"id\"]==\"$ID\" and a[\"bucket\"]==\"review\" for a in d[\"agents\"])'"
slen=$("$M" show --json "$ID" | json "len(d.get('summary') or '')")
check "latest update bounded to <= 1200 chars (D-027)" '[ "$slen" -gt 0 ] && [ "$slen" -le 1200 ]'

out=$(ingest claude '{not json'); rc=$?
check "malformed hook exits 0 silently" '[ $rc -eq 0 ] && [ -z "$out" ]'
sleep 1
check "malformed hook journaled as hook.rejected" \
  '"$M" events --json | json "any(e[\"event_type\"]==\"hook.rejected\" for e in d[\"events\"])" | grep -q True'
check "events never expose prompt text (D-033)" '! "$M" events --json | grep -q "summarize"'

echo "# rename and restart identity"
"$M" rename "$ID" "spec smoke" >/dev/null 2>&1
check "rename sets user_name" "wait_for 'any(a[\"id\"]==\"$ID\" and a.get(\"user_name\")==\"spec smoke\" for a in d[\"agents\"])'"
# `server restart` must itself succeed and replace the process. A failed
# restart is recorded as a failure, never papered over with stop/start.
# Identity across a plain stop/start cycle is checked separately below.
before=$(server_pid)
"$M" server restart --discovery=false >/dev/null 2>&1; restart_rc=$?
after=$(server_pid)
check "server restart exits 0 (LIFE-020)" '[ $restart_rc -eq 0 ]'
check "server restart replaced the process (new PID)" \
  '[ -n "$before" ] && [ -n "$after" ] && [ "$before" != "$after" ]'
check "restarted server honoured --discovery=false" \
  '[ $restart_rc -eq 0 ] && "$M" server status --json 2>/dev/null | json "all(p[\"status\"]==\"disabled\" for p in d[\"providers\"])" | grep -q True'
check "id and user_name preserved across restart (INV-006)" \
  "[ \$restart_rc -eq 0 ] && [ \"\$before\" != \"\$after\" ] && wait_for 'any(a[\"id\"]==\"$ID\" and a.get(\"user_name\")==\"spec smoke\" for a in d[\"agents\"])'"
# Keep the remaining checks meaningful if restart failed (already counted).
"$M" server status >/dev/null 2>&1 || start

echo "# daemon down"
"$M" server stop >/dev/null 2>&1
check "stop removes socket and pid file" '[ ! -e "$D/run/noc.sock" ] && [ ! -e "$D/run/noc.pid" ]'
out=$(ingest codex "{\"hook_event_name\":\"SessionStart\",\"session_id\":\"$X1\",\"cwd\":\"/tmp\"}"); rc=$?
check "ingest while server down exits 0 silently" '[ $rc -eq 0 ] && [ -z "$out" ]'
lsout=$("$M" ls 2>&1); rc=$?
check "client reports server down with exit 1" '[ $rc -eq 1 ] && echo "$lsout" | grep -q "Is the server running"'
start; rc=$?
check "server --bg after stop exits 0" '[ $rc -eq 0 ]'
check "spooled hook projected after stop/start" \
  "wait_for 'any(a[\"native_session_id\"]==\"$X1\" and a[\"provider\"]==\"codex\" for a in d[\"agents\"])'"
check "id and user_name preserved across stop/start (INV-006)" \
  "wait_for 'any(a[\"id\"]==\"$ID\" and a.get(\"user_name\")==\"spec smoke\" for a in d[\"agents\"])'"

echo "# session end"
ingest claude "{\"hook_event_name\":\"SessionEnd\",\"session_id\":\"$C1\",\"reason\":\"exit\"}" >/dev/null
check "SessionEnd hides named row from default list" \
  "wait_for 'not any(a[\"id\"]==\"$ID\" for a in d[\"agents\"])'"
check "named ended row retained in --all as not_live (D-041)" \
  "wait_for 'any(a[\"id\"]==\"$ID\" and a[\"live_state\"]==\"not_live\" and a[\"bucket\"]==\"quiet\" for a in d[\"agents\"])' all"
ingest codex "{\"hook_event_name\":\"SessionStart\",\"session_id\":\"$X2\",\"cwd\":\"/tmp\"}" >/dev/null
wait_for "any(a[\"native_session_id\"]==\"$X2\" for a in d[\"agents\"])" >/dev/null
ingest codex "{\"hook_event_name\":\"SessionEnd\",\"session_id\":\"$X2\"}" >/dev/null
check "unprompted unnamed ended row dropped entirely (D-047)" \
  "wait_for 'not any(a[\"native_session_id\"]==\"$X2\" for a in d[\"agents\"])' all"

echo
echo "passed: $pass  failed: $fail"
[ "$fail" -eq 0 ]
