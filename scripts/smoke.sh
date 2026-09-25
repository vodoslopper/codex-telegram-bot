#!/usr/bin/env bash
# smoke.sh — run the *compiled binary* end to end against a stub Telegram API and
# a fake Codex CLI.
#
# `go test` covers the packages, but it talks to an in-memory fake and so never
# exercises cmd/bot/main.go: the startup probes, the single-poller lock, the
# configuration cross-checks, the signal handling and the shutdown path. This
# script does, without a bot token, a network, or a Codex account.
#
# Usage: scripts/smoke.sh            (builds, runs, reports)
#        KEEP=1 scripts/smoke.sh     (leaves the temporary directories in place)
#
# Exit status is 0 when a full turn was observed: update received, Codex invoked
# with the expected argv, reply delivered back through the stub API.
set -euo pipefail

cd "$(dirname "$0")/.."
root="$PWD"

say() { printf '\n=== %s\n' "$*"; }
die() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }

tmp="$(mktemp -d)"
cleanup() {
	if [[ "${KEEP:-0}" == "1" ]]; then
		echo "keeping $tmp"
	else
		rm -rf "$tmp"
	fi
}
trap cleanup EXIT

# --- a workspace that satisfies the Git repository requirement --------------
ws="$tmp/workspace"
install -d -m 0755 "$ws/.git"
echo "hello" > "$ws/README.md"

state="$tmp/state"
home="$tmp/codex-home"
install -d -m 700 "$state" "$home"

# --- a fake Codex CLI -------------------------------------------------------
# It answers the three things the bot asks of it: --version at startup,
# `login status` for the credential probe, and `exec`/`exec resume` for a turn.
fakebin="$tmp/bin"
install -d -m 0755 "$fakebin"
artifacts="$tmp/artifacts"
install -d -m 0700 "$artifacts"

cat > "$fakebin/codex" <<FAKE
#!/usr/bin/env bash
set -u
ART='$artifacts'
case "\$1" in
  --version) echo "codex-cli 0.0.0-smoke"; exit 0 ;;
  login)     echo "Logged in using ChatGPT"; exit 0 ;;
esac
n=\$(cat "\$ART/count" 2>/dev/null || echo 0); n=\$((n+1)); printf '%s' "\$n" > "\$ART/count"
: > "\$ART/argv.\$n"
for a in "\$@"; do printf '%s\0' "\$a" >> "\$ART/argv.\$n"; done
printf '%s' "\$PWD" > "\$ART/cwd.\$n"
env | LC_ALL=C sort > "\$ART/env.\$n"
cat <<'JSONL'
{"type":"thread.started","thread_id":"0199a213-81c0-7800-8aa1-000000000001"}
{"type":"turn.started"}
{"type":"item.completed","item":{"id":"i1","type":"agent_message","text":"Smoke test answer: the repository has a README."}}
{"type":"turn.completed","usage":{"input_tokens":10,"output_tokens":5}}
JSONL
exit 0
FAKE
chmod 0700 "$fakebin/codex"

# --- build ------------------------------------------------------------------
say "building"
go build -o "$tmp/codex-telegram-bot" ./cmd/bot
echo "built $tmp/codex-telegram-bot"

# --- stub Telegram API ------------------------------------------------------
say "starting the stub Telegram API"
sent="$tmp/sent.jsonl"
: > "$sent"
port_file="$tmp/port"
STUB_SENT_FILE="$sent" STUB_UPDATE_ID=1001 \
	python3 scripts/stub-telegram-api.py 0 > "$port_file" &
stub_pid=$!
trap 'kill "$stub_pid" 2>/dev/null || true; cleanup' EXIT

for _ in $(seq 1 50); do
	[[ -s "$port_file" ]] && break
	sleep 0.1
done
port="$(cat "$port_file")"
[[ -n "$port" ]] || die "the stub API did not report a port"
echo "stub listening on 127.0.0.1:$port"

# --- run the bot ------------------------------------------------------------
say "running the bot"
log="$tmp/bot.log"
(
	export TELEGRAM_BOT_TOKEN="123456:smoke-test-token-value-not-real"
	export ALLOWED_TELEGRAM_USER_IDS="111"
	export BOT_WORKSPACE="$ws"
	export BOT_STATE_DIR="$state"
	export BOT_CODEX_BIN="$fakebin/codex"
	export CODEX_HOME="$home"
	export BOT_TELEGRAM_API_BASE="http://127.0.0.1:$port"
	export BOT_LOG_LEVEL=debug
	export BOT_ACK_AFTER=0s
	"$tmp/codex-telegram-bot"
) > "$log" 2>&1 &
bot_pid=$!

# Let it work. The lock check below needs the first instance alive, so the
# shutdown comes after the assertions.
sleep 8

# --- assertions -------------------------------------------------------------
say "checking the startup log"
grep -q 'level=INFO msg=starting' "$log"            || die "no startup line\n$(cat "$log")"
grep -q 'codex_home=.*codex-home' "$log"            || die "CODEX_HOME was not logged"
grep -q 'msg="codex cli found"' "$log"              || die "the codex probe did not run"
grep -q 'codex-cli 0.0.0-smoke' "$log"              || die "the codex version was not reported"
grep -q 'msg="codex login ok"' "$log"               || die "the login probe did not succeed"
grep -q 'msg="webhook cleared"' "$log"              || die "the webhook was not cleared"
grep -q 'msg="connected to Telegram"' "$log"        || die "getMe did not succeed"
grep -q 'bot_username=stub_bot' "$log"              || die "the bot username was not recorded"
grep -q 'msg="holding the single-poller lock"' "$log" || die "the poller lock was not taken"
grep -q 'msg="polling Telegram for updates"' "$log" || die "the poller did not start"
grep -q 'msg="a codex turn succeeded"' "$log"       || die "no turn succeeded\n$(tail -40 "$log")"
echo "startup and the turn were logged as expected"

if grep -q 'smoke-test-token-value' "$log"; then
	die "the bot token appeared in the log"
fi
echo "the token is not in the log"

say "checking the delivered reply"
[[ -s "$sent" ]] || die "nothing was sent to Telegram\n$(tail -40 "$log")"
cat "$sent"
grep -q 'Smoke test answer' "$sent" || die "the Codex reply was not delivered"
grep -q '"chat_id": *111' "$sent"   || die "the reply went to the wrong chat"
echo "the reply reached the right chat"

say "checking the Codex invocation"
n="$(cat "$artifacts/count" 2>/dev/null || echo 0)"
[[ "$n" == "1" ]] || die "codex was invoked $n time(s), want 1"
tr '\0' '\n' < "$artifacts/argv.1"
expected=$'exec\n--json\n--strict-config\n--sandbox\nworkspace-write\n--\nSummarize this repository'
got="$(tr '\0' '\n' < "$artifacts/argv.1")"
[[ "$got" == "$expected" ]] || die "unexpected argv:\n$got"
cwd="$(cat "$artifacts/cwd.1")"
[[ "$cwd" == "$ws" ]] || die "codex ran in $cwd, want $ws"
if grep -q 'TELEGRAM_BOT_TOKEN' "$artifacts/env.1"; then
	die "the bot token reached the Codex child environment"
fi
grep -q "^CODEX_HOME=$home\$" "$artifacts/env.1" || die "CODEX_HOME was not passed to the child"
echo "argv, cwd and the scrubbed child environment are all correct"

say "checking the persisted state"
ls -la "$state"
[[ -f "$state/bot.db" ]] || die "no database was created"
perm="$(stat -c '%a' "$state/bot.db")"
[[ "$perm" == "600" ]] || die "bot.db has mode $perm, want 600"
echo "bot.db exists with mode $perm"

say "checking that a second poller is refused while the first runs"
kill -0 "$bot_pid" 2>/dev/null || die "the bot exited before the lock could be tested"
second_log="$tmp/second.log"
set +e
TELEGRAM_BOT_TOKEN="123456:smoke-test-token-value-not-real" \
ALLOWED_TELEGRAM_USER_IDS="111" \
BOT_WORKSPACE="$ws" \
BOT_STATE_DIR="$state" \
BOT_CODEX_BIN="$fakebin/codex" \
CODEX_HOME="$home" \
BOT_TELEGRAM_API_BASE="http://127.0.0.1:$port" \
	timeout 20 "$tmp/codex-telegram-bot" > "$second_log" 2>&1
second_rc=$?
set -e
cat "$second_log"
[[ "$second_rc" != "0" ]] || die "a second poller started successfully; two pollers would steal updates"
grep -q 'already polling' "$second_log" || die "the refusal does not explain itself: $(cat "$second_log")"
echo "a second instance is refused with a clear message"

say "sending SIGTERM"
kill -TERM "$bot_pid" 2>/dev/null || true
for _ in $(seq 1 100); do
	kill -0 "$bot_pid" 2>/dev/null || break
	sleep 0.1
done
if kill -0 "$bot_pid" 2>/dev/null; then
	kill -KILL "$bot_pid" 2>/dev/null || true
	die "the bot ignored SIGTERM"
fi
kill "$stub_pid" 2>/dev/null || true
grep -q 'shutdown complete' "$log" || die "the shutdown was not clean:\n$(tail -20 "$log")"
echo "the bot shut down cleanly on SIGTERM"

printf '\n=== SMOKE TEST PASSED\n'
