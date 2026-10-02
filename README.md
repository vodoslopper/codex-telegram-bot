# codex-telegram-bot

Drive a locally installed [Codex CLI](https://learn.chatgpt.com/docs/codex/cli)
from a private Telegram chat.

You message the bot, the bot runs one `codex exec` turn in a Git repository you
chose, and the final agent message comes back as plain Telegram text. Sessions
persist, so a conversation continues across bot restarts. It is built for one
person, or a handful of explicitly allowlisted people, on one Linux host.

- **No inbound network access.** Telegram long polling only: no webhook, no
  public address, no open port, no TLS certificate to serve.
- **Its own Codex home.** The bot creates and resumes *its own* sessions under a
  dedicated `CODEX_HOME`. It refuses to start if that is your interactive
  `~/.codex`, so it can never list, resume or archive your personal sessions.
- **Allowlist by numeric id.** `from.id` is the only identity used. Usernames are
  never consulted — they can be renamed by anyone.
- **No shell.** Codex is spawned with `exec.CommandContext` and an argument
  array. Your message is one argv element behind a `--` terminator, so
  `; rm -rf ~ #` is a string Codex reads, not a command line.

```
Telegram ──getUpdates──> bot ──exec.CommandContext──> codex exec [--json]
                            │                              │
                            │<────────── JSONL ────────────┘
                            └── SQLite: sessions, thread ids, selections,
                                processed update ids, turn outcomes
```

---

## Contents

- [Quick start](#quick-start)
- [Host setup](#host-setup)
- [Configuration](#configuration)
- [Using the bot](#using-the-bot)
- [The exact Codex command lines](#the-exact-codex-command-lines)
- [How it works](#how-it-works)
- [Deliberate implementation choices](#deliberate-implementation-choices)
- [Security model](#security-model)
- [Backup and restore](#backup-and-restore)
- [Known limitations](#known-limitations)
- [What was actually tested](#what-was-actually-tested)
- [Project layout](#project-layout)

---

## Quick start

```sh
git clone https://github.com/vodoslopper/codex-telegram-bot.git
cd codex-telegram-bot

go build ./...            # compiles
go test ./...             # the whole suite; no network, no real account
go vet ./...

go build -o "$HOME/.local/bin/codex-telegram-bot" ./cmd/bot
```

After completing [Host setup](#host-setup), run it in the foreground with the
environment inline (note the leading space, so the token does not land in your
shell history):

```sh
 TELEGRAM_BOT_TOKEN='123456:real-token' \
 ALLOWED_TELEGRAM_USER_IDS='111111111' \
 BOT_WORKSPACE="$HOME/codex-bot-workspace" \
 BOT_STATE_DIR="$HOME/.local/share/codex-telegram-bot" \
 BOT_CODEX_BIN="$(command -v codex)" \
 CODEX_HOME="$HOME/.local/share/codex-telegram-bot/codex" \
 "$HOME/.local/bin/codex-telegram-bot"
```

The bot fails at startup, with a message naming every problem it found, if any of
that is missing or inconsistent. It does not start half-configured.

For the real thing, use the environment file and the systemd user unit:
[.env.example](.env.example) and
[deploy/systemd/codex-telegram-bot.service](deploy/systemd/codex-telegram-bot.service).

---

## Host setup

Assumes a Linux host with `systemd --user`, Go, an existing Git repository, and
that you do everything below as **one unprivileged account** — the same account
that logs Codex in and runs the service. If those differ, every turn fails as
unauthenticated, because the credential cache belongs to `CODEX_HOME`.

### 1. Create the Telegram bot

Talk to [@BotFather](https://t.me/BotFather), send `/newbot`, keep the token
private. You do not need to disable privacy mode or add the bot to any group: it
only ever reads private chats.

### 2. Find your numeric user id

The allowlist takes numbers, not usernames. First create the environment file
and put the BotFather token in `TELEGRAM_BOT_TOKEN`:

```sh
install -d -m 700 "$HOME/.config/codex-telegram-bot"
cp .env.example "$HOME/.config/codex-telegram-bot/bot.env"
chmod 600 "$HOME/.config/codex-telegram-bot/bot.env"
$EDITOR "$HOME/.config/codex-telegram-bot/bot.env"
```

Before the bot is running, the simplest local method is a one-time `getUpdates`
against your own token. Send yourself a message to the bot first, then:

```sh
# Reads the token from the env file, never from argv, so it stays out of `ps`
# and out of your shell history.
. "$HOME/.config/codex-telegram-bot/bot.env"
printf 'url = "https://api.telegram.org/bot%s/getUpdates"\n' "$TELEGRAM_BOT_TOKEN" |
  curl --silent --config - | python3 -m json.tool | grep -A3 '"from"'
unset TELEGRAM_BOT_TOKEN
```

`"id"` under `"from"` in your own message is the number to allowlist. Any
reputable ID lookup bot works too, but it necessarily sees your messages, so a
local query is the better trade.

### 3. Delete any existing webhook

A webhook and a poller are mutually exclusive: while a webhook is configured,
`getUpdates` answers `409` and the bot looks healthy while receiving nothing.

The bot deletes it at startup by default (`BOT_DELETE_WEBHOOK=true`), without
dropping pending updates. To do it by hand, use the helper, which keeps the token
out of your history and out of `ps`:

```sh
scripts/delete-webhook.sh -f "$HOME/.config/codex-telegram-bot/bot.env"
```

It prints the webhook state before and after. **Do not** run
`curl https://api.telegram.org/bot<TOKEN>/deleteWebhook` by hand: that puts the
token in your shell history and in the process list.

### 4. Install and check Codex

Follow the [official installation
instructions](https://learn.chatgpt.com/docs/codex/cli). For a user install:

```sh
curl -fsSL https://chatgpt.com/codex/install.sh | sh
command -v codex && codex --version
```

This project was developed and verified against **codex-cli 0.156.1**. Other
versions work as long as `codex exec` and `codex exec resume` accept the flags in
[The exact Codex command lines](#the-exact-codex-command-lines); re-check them
with the commands in that section after an upgrade.

### 5. Directories and the isolated Codex home

```sh
install -d -m 700 "$HOME/.config/codex-telegram-bot" \
                  "$HOME/.local/share/codex-telegram-bot" \
                  "$HOME/.local/share/codex-telegram-bot/codex"
install -d -m 755 "$HOME/.local/bin" "$HOME/codex-bot-workspace"
git -C "$HOME/codex-bot-workspace" init
```

If the bot should work on an existing repository instead, point `BOT_WORKSPACE`
at that checkout. Give the account only the repositories the agent should be
allowed to modify — under `workspace-write` Codex can change files in it.

### 6. Log Codex in **under the bot's CODEX_HOME**

```sh
CODEX_HOME="$HOME/.local/share/codex-telegram-bot/codex" codex login --device-auth
CODEX_HOME="$HOME/.local/share/codex-telegram-bot/codex" codex login status
```

Two things worth knowing:

- `codex login status` exits **0** whether or not you are logged in; only its
  text differs. The bot parses the output rather than the exit code, and warns at
  startup if it reads "Not logged in".
- `CODEX_HOME` must already exist, or Codex refuses to load its configuration.
  The bot creates it `0700` if it is missing.

If device-code login is unavailable, follow the [authentication
documentation](https://learn.chatgpt.com/docs/auth) for a supported alternative.

### 7. Configure

Open the environment file you created in step 2 and replace the sample user id
with your numeric id. Fill in the four absolute paths as well:

```sh
$EDITOR "$HOME/.config/codex-telegram-bot/bot.env"
```

Fill in the real token and your numeric id, and set the four absolute paths.
`BOT_CODEX_BIN` must be the output of `command -v codex` **for this account**,
and `CODEX_HOME` must be exactly the value used for `codex login` above.

### 8. Build, install, run

```sh
cd "$HOME/codex-telegram-bot"
go test ./...
go build -o "$HOME/.local/bin/codex-telegram-bot" ./cmd/bot

install -d -m 700 "$HOME/.config/systemd/user"
cp deploy/systemd/codex-telegram-bot.service "$HOME/.config/systemd/user/"
# edit the four paths in the unit if they differ from the defaults
systemctl --user daemon-reload
systemctl --user enable --now codex-telegram-bot.service
systemctl --user status codex-telegram-bot.service
journalctl --user -u codex-telegram-bot.service -f
```

To keep the service alive after you log out, an administrator runs
`sudo loginctl enable-linger YOURUSER`.

Do not also run the bot in the foreground: it takes an exclusive lock on
`$BOT_STATE_DIR/poller.lock` and the second process refuses to start. That is
deliberate — two pollers share one Telegram offset and would steal updates from
each other.

### 9. Check it

In the private chat: `/start`, then `/new test`, then something simple like
*what is in this repository?* Then confirm the session survives a restart:

```sh
systemctl --user restart codex-telegram-bot.service
```

and send another message. It should answer with the earlier conversation still in
context, and the journal should show a `resume` of the same thread id.

---

## Configuration

Everything comes from the environment, so the same binary works under systemd,
a container, or a shell, and no secret ever appears on a command line.
[.env.example](.env.example) documents each one in place; this is the summary.

### Required

| Variable | Meaning |
| --- | --- |
| `TELEGRAM_BOT_TOKEN` | From @BotFather. Must look like `<id>:<secret>`, no whitespace. |
| `ALLOWED_TELEGRAM_USER_IDS` | Numeric ids, comma or space separated. At least one. Usernames are rejected. |
| `BOT_WORKSPACE` | Absolute path to an existing **Git repository**. Codex runs here. |
| `BOT_STATE_DIR` | Absolute path for the SQLite database and the poller lock. Must be **outside** the workspace. Created `0700`. |
| `BOT_CODEX_BIN` | The Codex executable. A bare name is resolved on `PATH`. |
| `CODEX_HOME` | Absolute, **not** `~/.codex`, **outside** the workspace. Created `0700`. |

### Optional

| Variable | Default | Meaning |
| --- | --- | --- |
| `BOT_SANDBOX_MODE` | `workspace-write` | `workspace-write` or `read-only`. `danger-full-access` is refused outright. |
| `BOT_CODEX_MODEL` | `gpt-6-sol` | Baseline model; `gpt-6-sol` or `gpt-6-luna`. Passed as `-m`. |
| `BOT_CODEX_STRICT_CONFIG` | `true` | Pass `--strict-config`. |
| `BOT_CHECK_CODEX_LOGIN` | `true` | Probe `codex login status` at startup and warn. |
| `BOT_TURN_TIMEOUT` | `15m` | Per-turn deadline, 30s..24h. |
| `BOT_QUEUE_TIMEOUT` | `5m` | How long a message waits for the workspace before "busy". |
| `BOT_MAX_CONCURRENT_UPDATES` | `8` | Turns queued or running at once. |
| `BOT_ACK_AFTER` | `10s` | Send "still working" after this long. `0` disables it. |
| `BOT_SHUTDOWN_GRACE` | `20s` | Let in-flight turns finish this long before cancelling. |
| `BOT_POLL_LIMIT` | `100` | Updates per `getUpdates` (Telegram maximum 100). |
| `BOT_POLL_TIMEOUT_SEC` | `25` | Server-side long-poll wait. |
| `BOT_DELETE_WEBHOOK` | `true` | Clear any webhook at startup, keeping pending updates. |
| `BOT_TELEGRAM_API_BASE` | `https://api.telegram.org` | Only for testing against a local API server. |
| `BOT_MAX_REPLY_CHARS` | `4096` | Chunk size for splitting long replies. |
| `BOT_MAX_STDERR_BYTES` | `65536` | Retained Codex stderr tail. |
| `BOT_MAX_EVENT_BYTES` | `8388608` | Longest accepted JSONL event line. |
| `BOT_LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error`. |
| `BOT_LOG_PROMPTS` | `false` | Log prompt **text**. Off by default: the log carries lengths only. |

Fixed constants (not environment variables, because there is no good reason to
change them): the SIGTERM→SIGKILL grace is 5s, the typing indicator refreshes
every 4s, and a failure message quotes at most 600 characters of stderr.

### What makes startup fail

Missing or malformed values above, plus these cross-checks, all reported together
in one message:

- `BOT_WORKSPACE` is not a Git repository (no `.git` entry).
- `BOT_STATE_DIR` or `CODEX_HOME` is inside `BOT_WORKSPACE`.
- `CODEX_HOME` is your interactive `~/.codex`.
- `BOT_CODEX_BIN` does not exist, is a directory, is not executable, or does not
  run (`codex --version` is executed once at startup and the version logged).
- `BOT_SANDBOX_MODE=danger-full-access`.
- A webhook cannot be removed *and* the token was rejected (401 is fatal; a
  network failure is not — the poller retries).

---

## Using the bot

Private chats only. Groups, supergroups and channels are dropped silently, and
messages from anybody not on the allowlist are dropped without a reply —
answering them would confirm the bot exists to strangers.

The bot registers its main commands with Telegram when it connects. Open the
chat's command menu or type `/` to choose one. `/sessions` shows buttons for the
eight most recent sessions; tap one to switch in that chat or topic. The session
button message normally updates to show the new selection, so repeated switches
do not add confirmation messages to the chat. `/model` shows buttons for the
current chat or topic and your default; choosing one normally updates the model
message in place. Text commands still work for every option, including older
sessions beyond the button list.
Ordinary messages remain free-form Codex prompts. Button presses follow the
same user allowlist and private-chat rules as messages.

| Command | Effect |
| --- | --- |
| `/start`, `/help` | Commands, the workspace in use, the sandbox, the current selection. |
| `/new [name]` | Create a session, select it. Name may contain spaces. |
| `/sessions [all]` | List **your** sessions; `all` includes archived ones. `*` marks the selected one. |
| `/use <id>` | Switch this chat (and topic) to one of your sessions. |
| `/session` | The selected session, workspace, Codex thread, and running/idle status. `/status` and `/current` are aliases. |
| `/usage` | Last reported context use for the selected session, plus live account rate limits when available. |
| `/model` | Show the effective GPT-6 model in this chat or topic. |
| `/model luna` or `/model sol` | Save a model override for this chat or topic. `/model reset` restores inheritance. |
| `/model default luna` or `/model default sol` | Save your default model for chats and topics without an override. `/model default reset` restores `BOT_CODEX_MODEL`. |
| `/rename <name>` | Rename the session selected in this chat or topic. Name may contain spaces. |
| `/rename <id> <name>` | Rename one of your sessions by ID. |
| `/archive <id>` | Hide and deselect a session in every chat and topic. Codex history is untouched; `/unarchive <id>` brings it back. |
| `/stop` | Cancel the turn running **in this chat**. The session and its thread survive. |
| anything else | Becomes a Codex prompt. If nothing is selected, a session is created first and you are told its id. |

`/usage` does not start a Codex turn. Its context figure comes from the selected
thread's latest saved Codex token report. The bot asks Codex's local app-server
for current 5-hour and weekly limits; if that experimental interface is
unavailable, it shows the saved limits from the same thread and labels their
timestamp. Reset times are shown in UTC. Without a completed turn in the
selected session, context use is unavailable.

Photos, documents, audio (including voice notes), and video (including video
notes) can also start a turn. A caption is used as the prompt; without one, the
bot asks Codex to inspect the attachment. Photos and image documents are passed
through Codex's `--image` option on the receiving turn. Other files are made
available at a private path in the workspace and named in the prompt. Later
turns in the same session are given the retained file path, so Codex can inspect
it again without a separate model call. Codex judges whether each completed
follow-up turn relates to the file; the bot removes it after the third unrelated
turn. Related turns do not count, and turns in other sessions do not count.
Archiving a session removes all its retained files and clears its selections.
You can explicitly select an archived session again with `/use`; an attachment
sent to it is removed after that turn. These counts and paths survive a bot
restart. Telegram's public Bot API limits downloads to 20 MB;
larger files get an error before Codex runs. Codex needs suitable tools in the
workspace to inspect or transcribe audio and video files.

When Codex creates a file for the user, it includes a
`[[telegram-file:/absolute/path/in/workspace]]` line in its final answer. The bot
uploads each named file as a Telegram document, then sends the answer without
those lines. Documents preserve exact bytes, including PNG transparency. Up to
five nonempty files of at most 50 MB each can be sent per turn. Files must be in
the configured workspace and created or modified during that turn; links out of
the workspace and files under `.git` are rejected. Files remain in the workspace
after delivery. If an upload fails, the bot retries the stored answer while the
file remains available. Invalid file references are skipped with a note in the
text answer; they are not retried indefinitely.

The model choice is read when each turn starts. It also applies when an existing
Codex session resumes. Choices are stored per allowed user; topics have separate
overrides.

### Two different kinds of id

The bot shows you short ids like `s7k3qm`; Codex threads are UUIDs like
`0199a213-81c0-7800-8aa1-bbab2a035a53`. They are deliberately different things:

- The alphabet drops `0 1 i l o`, so an id survives being read aloud or copied by
  hand.
- The `s` prefix is not a hexadecimal digit, so a bot id can **never** be parsed
  as a UUID — and can never be pasted into `codex exec resume` to address
  somebody else's conversation.
- It contains no `-`, so it can never be mistaken for a command-line flag.

`codex exec resume` also accepts a thread *name*. The bot refuses to store or
pass anything that is not a 36-character UUID, so a name can never resolve to a
different session than the one you meant.

### Topics

Private chats with topics are supported: each topic keeps its own selection, and
a reply is sent back to the topic it came from. `/session` in a topic says which
topic you are in.

### What a long reply looks like

Replies are sent as plain text, split at paragraph, line or word boundaries into
chunks of at most 4096 **UTF-16 code units** — Telegram counts in UTF-16, so an
emoji costs two and counting Go runes would produce oversized messages that the
API rejects. No chunk ever splits a surrogate pair. Raw Codex JSONL, hidden
reasoning and tool logs are never forwarded.

---

## The exact Codex command lines

These are the argv arrays the bot builds, verified against **codex-cli 0.156.1**
on Linux. `internal/codexcli/argv_test.go` pins them, so a change fails a test.

**First turn of a session** (no stored thread id):

```
codex exec --json --strict-config --sandbox workspace-write -m gpt-6-sol -- <PROMPT>
```

with the effective model flag inserted before `--`:

```
codex exec --json --strict-config --sandbox workspace-write -m <MODEL> -- <PROMPT>
```

**Every later turn of that session** (resuming the exact stored UUID):

```
codex exec resume <STORED_THREAD_UUID> --json --strict-config -c sandbox_mode="workspace-write" -m gpt-6-sol -- <PROMPT>
```

Both run with the child process's working directory set to `BOT_WORKSPACE`,
`CODEX_HOME` set in its environment, and stdin attached to `/dev/null`.

### Why the two differ — and how to re-verify

`codex exec resume` in 0.156.1 accepts **neither** `--sandbox/-s` **nor**
`--cd/-C`:

```console
$ codex exec resume <uuid> -s workspace-write --json hello
error: unexpected argument '-s' found

  tip: to pass '-s' as a value, use '-- -s'

Usage: codex exec resume [OPTIONS] [SESSION_ID] [PROMPT]
```

So:

- **The sandbox** for a resumed turn travels as the documented config key,
  `-c sandbox_mode="workspace-write"`. The value is quoted so it parses as TOML
  rather than falling back to Codex's raw-string path. `sandbox_mode` is the same
  key `config.toml` uses, and `--strict-config` makes an unrecognised key a hard
  error instead of a silently dropped sandbox override.
- **The working directory** is set on the child process (`cmd.Dir`) for *both*
  paths, because that is the one mechanism `resume` honours — and Codex filters
  recorded sessions by cwd unless `--all` is given, so a resumed turn run from
  the wrong directory fails with *"Not inside a trusted directory"*.
- `--last` and `--all` are never used: either could select a session other than
  the one this Telegram user owns. The bot resumes only the UUID it recorded from
  that session's own `thread.started` event.
- `--ephemeral` is never used: threads must survive a bot restart.
- `--dangerously-bypass-approvals-and-sandbox`,
  `--dangerously-bypass-hook-trust`, `--approve-for-me` and
  `--skip-git-repo-check` are never used, and a test asserts none of them can
  appear in the argv.
- `--` terminates option parsing, so a prompt beginning with `-` is still a
  prompt. Verified: `codex exec --json -- "-sandbox danger-full-access"` starts a
  thread instead of changing the sandbox.

Re-check all of this on your own host after any Codex upgrade:

```sh
codex --version
codex exec --help
codex exec resume --help
```

If `resume` gains a `--sandbox` flag, prefer it over the `-c` override and update
`(*Runner).Argv` plus `argv_test.go`.

### The JSONL the bot understands

From the [non-interactive mode
documentation](https://learn.chatgpt.com/docs/non-interactive-mode), and observed
from 0.156.1:

```jsonl
{"type":"thread.started","thread_id":"0199a213-81c0-7800-8aa1-bbab2a035a53"}
{"type":"turn.started"}
{"type":"item.started","item":{"id":"item_1","type":"command_execution","command":"bash -lc ls","status":"in_progress"}}
{"type":"item.completed","item":{"id":"item_3","type":"agent_message","text":"Repo contains docs, sdk, and examples directories."}}
{"type":"turn.completed","usage":{"input_tokens":24763,"cached_input_tokens":24448,"output_tokens":122,"reasoning_output_tokens":0}}
```

A turn succeeds **only** when `turn.completed` arrives *and* an
`item.completed` with `item.type == "agent_message"` supplied non-empty text. The
last such message is the reply. Everything else is a failure:

| Situation | Stored status | Reported as |
| --- | --- | --- |
| `turn.completed` + agent message | `completed` | the message |
| `turn.failed` | `failed` | Codex's own reason |
| Nonzero exit | `failed` | exit code + reason + stderr tail |
| No `thread.started` | `failed` | "did not report a thread id" |
| No `turn.completed` | `failed` | "stopped before turn.completed" |
| No agent message | `failed` | "no final message" |
| `BOT_TURN_TIMEOUT` elapsed | `timeout` | the limit, and that the group was killed |
| `/stop` | `cancelled` | that the group was terminated |
| Workspace busy past `BOT_QUEUE_TIMEOUT` | `busy` | how to cancel or retry |
| Bot died mid-turn | `interrupted` | shown by `/session` after a restart |

Unknown event kinds are counted and ignored, never forwarded: Codex adds event
types between releases, and a new one must not break a working bot. `reasoning`
items are dropped without being read, so hidden reasoning is never stored, logged
or sent anywhere.

An `error` event is **not** treated as fatal on its own. Observed from 0.156.1:

```jsonl
{"type":"error","message":"Reconnecting... 2/5 (unexpected status 401 Unauthorized: ...)"}
```

That is a transient stream problem, and Codex can still recover and complete the
turn. Error events are collected as diagnostics; the verdict comes from
`turn.completed` / `turn.failed` and the exit status.

---

## How it works

### One message, at most one Codex run

The guarantee is enforced in three independent places, because the failure it
prevents — a retry quietly running a second agent turn that edits files — is the
worst thing this bot can do.

1. **The poller claims before it advances.** Each update is written to
   `processed_updates` *synchronously*, in the poller loop, before the offset
   moves past it. A redelivered update finds its row and is not executed.
2. **`turns.update_id` is `UNIQUE`.** Even a bug that skipped the claim cannot
   start a second process for one message: the insert fails first.
3. **The offset only moves forward.** `AdvanceOffset` refuses to rewind, so a bug
   cannot replay history.

A claim failure stops the batch: the offset is *not* advanced past an update the
bot failed to record, so Telegram redelivers it and the claim is retried.

**The remaining unavoidable ambiguity.** If the process dies after Codex accepted
a turn but before the outcome was recorded, the bot cannot know what happened.
Codex may have completed the work, edited files, and even produced an answer that
was lost. What the bot does:

- The turn row stays `running` and is flipped to `interrupted` at the next
  startup, with an explanation stored in the row.
- `/session` reports it, including a warning to check `git status` before
  continuing.
- The update was already claimed, so **the prompt is never re-issued**. The bot
  prefers to lose a reply over running the same file-editing task twice.
- If the turn *had* completed and only its delivery was unconfirmed
  (`turns.delivered = 0`), the bot retries the stored reply in the original chat
  and topic without touching Codex. It checks on startup and every 30 seconds,
  after the original delivery deadline has passed. For updates claimed before
  the topic-id migration, it uses a uniquely identifiable earlier session
  selection when available. Otherwise it sends a labeled recovery message to
  the recorded private chat, without guessing a topic. No manual database work
  is needed during migration.

That is the honest limit: execution is at most once. A Telegram response can
arrive after the bot loses the delivery acknowledgement, so retrying an
unconfirmed reply can occasionally duplicate its text or files.

### Concurrency

```
scope lock (chat + topic)  →  workspace lock  →  codex
```

- **At most one in-flight turn per session.**
- **Turns sharing a working tree are serialised.** With one `BOT_WORKSPACE` that
  makes the whole bot one turn at a time, which is the point: Codex edits files,
  and two agents in one checkout corrupt each other's work.
- Waiting is bounded by `BOT_QUEUE_TIMEOUT`; past that you get a clear "busy"
  answer naming `/stop`, rather than an unbounded queue you forgot about.
- Locks are channel-based, and Go's runtime hands channel waiters off in FIFO
  order, so one chat's messages run in arrival order.
- `BOT_MAX_CONCURRENT_UPDATES` caps queued-plus-running turns. Over the cap a
  message is refused immediately. The cap is checked **without blocking**, so the
  poller keeps polling and `/stop` always gets through even when the bot is
  saturated.

A test proves non-overlap by having the fake Codex timestamp its own start and
end, then checking the intervals.

### Cancellation

`/stop` cancels the turn, not the session: the Codex thread id is kept and the
next message resumes it.

The child gets its own process group (`Setpgid`). A watchdog sends `SIGTERM` to
`-pgid`, then `SIGKILL` after the grace period, because Codex spawns a shell
which spawns the agent's commands — killing only the direct child would orphan
those. Go's own `cmd.Cancel` is a no-op here for the same reason; it would kill
the parent and leave the children.

Post-turn bookkeeping runs on a context **detached** from the turn's, so a
cancelled turn still records its status and still saves the thread id it learned.
(That was a real bug the test suite caught: `/stop` used to leave the row stuck
at `running` and throw the thread id away.)

### Restart

- Thread ids, session names, selections and the Telegram offset all live in
  SQLite and survive.
- `running` turns become `interrupted` and are reported.
- No prompt is re-issued at startup, ever.
- The stored thread id is refreshed from **every** turn's `thread.started`. If a
  future Codex version forks a new thread on resume, the mapping follows it and
  the chain stays intact; if it reuses the id, this is a no-op. Not updating
  would be the unsafe choice.

---

## Deliberate implementation choices

Things a reviewer might otherwise reasonably ask about.

**No Telegram framework.** The client is ~400 lines over `net/http`: `getUpdates`,
`sendMessage`, `sendChatAction`, `getMe`, `getWebhookInfo`, `deleteWebhook`. That
is the whole surface a long-polling bot needs, and owning it means owning the
offset handling, the retry/backoff and the token redaction — the three parts that
actually matter here. The only dependency in the module is
`modernc.org/sqlite` (pure Go, no cgo, so the binary builds anywhere).

**SQLite, not JSON files.** The deduplication guarantee needs a transactional
write that survives a crash. `synchronous=FULL` is set explicitly: the property
required is that a committed `processed_updates` row survives a *power cut*, not
just a process crash. One connection (`SetMaxOpenConns(1)`) removes `SQLITE_BUSY`
as a failure mode entirely; the write volume is a handful of rows per message.

**Timestamps are RFC 3339 UTC text, compared in Go.** Never in SQL: RFC 3339 with
a trimmed fractional part does not sort correctly as text
(`…00.5Z` < `…00Z`), so `ORDER BY created_at` would be a latent bug. Row counts
are small enough that sorting in Go costs nothing.

**Malformed JSONL is tolerated; a malformed *result* is not.** A stray non-JSON
line on stdout is counted, logged, and does not discard an otherwise complete
answer. But a stream that ends without `turn.completed` fails however plausible
its text looked, and a nonzero exit fails even if a reply was captured.

**An oversized JSONL line is skipped, not fatal.** One pathological event must not
throw away a good turn; the line is drained and counted.

**The child environment is scrubbed.** `TELEGRAM_*`, `BOT_*` and
`ALLOWED_TELEGRAM_*` are removed before Codex is spawned. Under
`workspace-write` Codex runs shell commands, so if the token were in its
environment, one prompt ("print your environment") would be enough to exfiltrate
it into a chat. Credentials Codex needs (`OPENAI_API_KEY`, `CODEX_API_KEY`,
`PATH`) are kept. A test asserts both halves.

**Stdin is `/dev/null`.** Codex reads stdin even when given a prompt argument,
and appends piped content as a `<stdin>` block. Your Telegram message must be the
whole prompt, so the child gets an immediate EOF. The fake Codex in the tests
runs `cat` on its stdin, so an open pipe would hang the suite rather than pass.

**Prompts are not persisted.** The database stores the prompt's *length* and the
argv with the prompt replaced by `<prompt>`. Replies are stored, because that is
what makes crash recovery work; prompts are not needed for anything and their
absence shrinks what a stolen database reveals. `BOT_LOG_PROMPTS` is off by
default for the same reason.

**Session ids are not capabilities.** Every query that reads or writes a session
carries `AND owner_user_id = ?`, and a session that belongs to somebody else
returns `ErrNotFound` — indistinguishable from one that does not exist, so ids
cannot be enumerated. `/use` checks ownership twice: once to produce a good error
message, and again inside the `INSERT … SELECT` that writes the selection, so
there is no window between check and write.

**`--strict-config` is on by default.** It costs a hard failure if your
`config.toml` has a key this Codex version does not know (a downgrade, say) and
buys the guarantee that the sandbox override was actually understood rather than
silently dropped. Silence in the wrong direction here means an unintended
sandbox.

**`danger-full-access` is not configurable.** The string is rejected by
`config.Load` and again by `codexcli.NewRunner`. A Telegram message reaching an
unsandboxed agent on your host is not a trade this bot makes; use a container if
you need that.

**A webhook is deleted, pending updates are not.** `deleteWebhook` is called with
`drop_pending_updates=false`, so messages sent while the bot was down still
arrive.

**The systemd unit is sandboxed less than you might expect, on purpose.**
Everything a unit sets is inherited by Codex and by every command the agent runs.
`MemoryDenyWriteExecute=` and `SystemCallFilter=` would break a JIT (node, a
JVM), and `ProtectSystem=strict` would make the repository the agent is supposed
to edit read-only. Codex's own sandbox is the control that scopes the agent; the
unit adds only the hardening that cannot break it. The reasoning is written in
the unit file too.

---

## Security model

Read this before adding anybody to the allowlist.

**A person who can prompt this bot can change files in `BOT_WORKSPACE`, as the
service account, within the Codex sandbox.** That is the entire point of it, and
it is why the allowlist should be one or two ids. Prompt injection applies: a
repository the agent reads can contain instructions that try to steer it.

What the bot guarantees:

- Only private chats. Groups and channels are dropped without a reply.
- Only allowlisted numeric `from.id` values, checked before any command is
  parsed, before the database is touched for that update, and before any process
  is started. Senders that are bots are dropped.
- A session cannot be read, renamed, archived, selected or run by another user,
  even with its exact id. Ownership is in the SQL, and "not yours" is
  indistinguishable from "does not exist".
- No shell. Codex is spawned with an argv array; the prompt is one element behind
  `--`.
- `workspace-write` at most. No approval bypass, no `danger-full-access`, no
  `--skip-git-repo-check`.
- The bot token and the allowlist never reach the Codex child process, the log, or
  a Telegram message. The log handler additionally masks any attribute named
  `token`, `secret`, `api_key`, `password` or `authorization`, so a future call
  site cannot leak one by accident.
- Errors shown to users are secret-scrubbed (API keys, bearer tokens,
  `key=value` credentials, URL userinfo) and length-bounded.
- The database, `BOT_STATE_DIR` and `CODEX_HOME` are `0600`/`0700`, re-asserted
  with `chmod` rather than left to the umask. A too-open directory is warned
  about at startup.
- `CODEX_HOME` cannot be your interactive `~/.codex`.

What it does **not** guarantee:

- Interactive approvals. `codex exec` is non-interactive, so a task needing one
  cannot ask you. Depending on the Codex version and configuration such a turn
  either fails, or the command is skipped and the agent works around it — the bot
  reports the failure with Codex's own reason. There is no approval button in
  Telegram, by design: an approval prompt relayed to a chat is an approval
  prompt that can be answered by anybody who gets hold of the phone.
- Anything about the *content* the agent writes. `git status` and `git diff` in
  the workspace are your review mechanism; that is why a repository is required.
- Rate limiting per user. The allowlist is assumed to be people you trust.

---

## Backup and restore

Back up **two things together**, or you get a bot with amnesia:

1. `BOT_STATE_DIR` — the SQLite database (sessions, ownership, thread ids,
   selections, offset, turn history) and the poller lock.
2. `CODEX_HOME` — the Codex credential cache and the session/rollout history the
   thread ids point at.

```sh
systemctl --user stop codex-telegram-bot.service
tar -C "$HOME" -czf "$HOME/codex-bot-backup-$(date -u +%Y%m%dT%H%M%SZ).tar.gz" \
    .local/share/codex-telegram-bot .config/codex-telegram-bot
chmod 600 "$HOME"/codex-bot-backup-*.tar.gz
systemctl --user start codex-telegram-bot.service
```

Both contain secrets (conversation replies; Codex access tokens), so treat the
archive like a password: `chmod 600`, and do not put it in the workspace, a
repository, or a world-readable backup share.

Stop the service first. Copying a live WAL-mode SQLite database can capture a
torn state; if you must back up while it runs, use
`sqlite3 "$BOT_STATE_DIR/bot.db" ".backup '/path/to/copy.db'"`, which takes a
consistent snapshot.

Restore: stop the service, untar over the same paths **as the same user**, check
the modes (`0700` for the directories, `0600` for `bot.db` and `bot.env`), start
the service. Sessions, selections and the offset come back; the next message
resumes its thread.

Restoring **only** the database gives you session ids that point at threads
Codex no longer has. That is handled, not fatal: the turn fails with
*"Codex no longer has the conversation this session points at"* and tells you to
`/new`. Restoring only `CODEX_HOME` leaves Codex holding threads the bot does not
know about — harmless, and unreachable.

---

## Known limitations

- **One workspace.** `BOT_WORKSPACE` is a single directory. A session records the
  workspace it was created in, but the bot cannot run turns in two repositories.
  Run two bots (different tokens, state dirs and `CODEX_HOME`s) if you need that.
- **One turn at a time.** Because Codex edits files, turns sharing the worktree
  are serialised. A burst of messages queues up to `BOT_QUEUE_TIMEOUT` and then
  gets a busy answer.
- **No approval relay.** See the security model. A task that needs an interactive
  approval cannot get one through the bot.
- **Other media.** Stickers and other unsupported message types get a hint.
  Edited messages are ignored: re-running an agent turn because somebody fixed a
  typo is the wrong default. Codex's relevance judgment is approximate. A crash
  during file staging or removal can leave an orphan `.codex-telegram-media-*`
  directory in the workspace; retained directories that still appear in the
  bot database must be left in place for follow-up turns.
- **At-most-once, with the ambiguity described above.** A crash after Codex
  accepted a turn and before the outcome was recorded loses that turn's reply,
  and the bot cannot tell you whether the file edits landed. `/session` will show
  the interruption.
- **Ordering across chats is not guaranteed.** Within one chat, messages run in
  arrival order. Across different chats the waiters are served in FIFO order too,
  but "arrival" means arrival at the lock, not at Telegram.
- **The offset is global.** Telegram gives one bot one update stream. Running two
  pollers with the same token would steal updates from each other; the
  `poller.lock` file exists to make that impossible by accident.
- **No `/undo`.** Reverting the agent's changes is `git` in the workspace, not a
  bot command.
- **Codex CLI version coupling.** The argv shape is pinned to what 0.156.1
  accepts. After an upgrade, re-run the `--help` checks above; `argv_test.go`
  will tell you if the expectations need updating.
- **Linux.** `Setpgid` and `flock` are used directly. A different OS needs a
  different process-group and locking strategy.

---

## What was actually tested

Be clear about this, because it affects how much to trust the first live run.

**Tested**, by `go test ./...` — 100% against fakes, no network, no real bot
token, no real Codex account, and no Codex turn ever run:

- The allowlist: an unauthorized user gets no reply, runs no Codex, creates no
  session, and cannot read or run somebody else's session by guessing its id.
- Ownership and isolation at the store level: every session query filters on the
  owner, and "not yours" is indistinguishable from "does not exist".
- Session create / list / rename / archive / unarchive, and switching with
  `/use`.
- The acceptance scenario end to end: `/new` → prompt → `exec` argv recorded and
  the thread UUID stored → second prompt → `resume <that exact UUID>` argv
  recorded → restart on the same directories → selection and thread intact →
  `/use` back to the first session.
- Duplicate updates: one Codex invocation and one turn row. Completed replies
  left undelivered after the update offset advances are retried **from the
  database** in their original chat or topic, without recomputing them.
- Command parsing, including `@mention` addressing and multi-word rename targets.
- Telegram message splitting: never over 4096 UTF-16 units, never a split
  surrogate pair, nothing lost, paragraph and line breaks preferred.
- Cancellation with a fake Codex process: `/stop` terminates the process group
  and a **grandchild** process is verified dead, the turn is recorded as
  `cancelled`, and the session keeps its thread id.
- Timeout, with the same grandchild check.
- Serialisation: three concurrent prompts produce three non-overlapping Codex
  invocations, proven from timestamps the fake CLI records itself.
- Busy handling, the concurrency cap, and the interrupted-turn recovery path.
- Secret handling: a stderr containing API keys, a bearer token and
  `OPENAI_API_KEY=` produces a user message with all of them masked; the bot
  token never appears in any error, including transport errors and errors that
  quote it back; prompts are absent from the log and from the database by
  default and present when `BOT_LOG_PROMPTS=true`.
- The Telegram client against `httptest`: parsing, `429 retry_after` honoured,
  5xx retried, 4xx not retried, cancellation interrupting a backoff, webhook
  handling, and `allowed_updates` restricted to messages.
- The poller: the offset advances only after updates are claimed, and a rejected
  token stops it.
- Configuration validation: every required variable, every range check, and each
  cross-check.
- The single-poller lock, including that the kernel releases it when the owning
  process is killed without cleanup.
- The lock and in-flight primitives directly: mutual exclusion under 12
  concurrent acquirers, independence between keys, cancellable waits, idempotent
  release, and FIFO handoff.

**Also tested**, by `scripts/smoke.sh`, which runs the *compiled binary* against a
stub Bot API and a fake Codex CLI — the one thing `go test` cannot reach, since it
never executes `main`:

- The startup probes, the configuration cross-checks, and the single-poller
  refusal (with a clear message rather than a crash).
- A full turn: update received, exact argv and working directory at Codex, reply
  delivered back through the API to the right chat.
- The token absent from the log and from the child environment; `bot.db` created
  `0600`; a clean shutdown on SIGTERM.

**Verified against the real Codex CLI** (0.156.1), but only up to the point where
credentials are needed — the probe used a throwaway `CODEX_HOME` with no login, so
Codex reached its API call and failed with `401`. No account was used and no
model call succeeded. This established: which flags `exec` and `exec resume`
accept and reject, that `--` terminates option parsing, that `resume` uses the
process working directory, the `thread.started` / `turn.started` / `error` event
shapes on the wire, that an `error` event can be transient, that Codex reads
stdin even with a prompt argument, and that `codex login status` exits 0 when not
logged in.

**Not tested**: a real Telegram bot token, a real authenticated Codex turn, the
full JSONL of a successful turn (only the documented sample and the observed
prefix), whether a resumed turn inherits the original session's sandbox policy or
takes the `-c` override (both give `workspace-write` here, so the difference is
not observable in this configuration), and `systemd --user` deployment.

So: expect the first live run to be the first real exercise of the Codex↔Telegram
seam. Start with `BOT_SANDBOX_MODE=read-only` and a scratch repository, send
something trivial, and read
`journalctl --user -u codex-telegram-bot.service -f` while you do.

---

## Project layout

```
cmd/bot/main.go                     startup checks, wiring, signals, shutdown
migrations/                         embedded SQL, applied in order, once each
internal/config/                    environment parsing and validation
internal/lockfile/                  flock-based single-poller lock
internal/store/                     SQLite: sessions, selections, turns, updates, offset
internal/telegram/                  Bot API client: polling, sending, retries, redaction
internal/codexcli/                  argv construction, JSONL parsing, supervised child process
internal/bot/                       gates, commands, turn orchestration, concurrency
internal/textsplit/                 UTF-16-aware Telegram message splitting
internal/sessid/                    short, unambiguous, non-UUID session ids
internal/redact/                    secret masking for logs and for user-visible errors
internal/testkit/                   the fake Codex CLI, the in-memory Telegram, fixtures
deploy/systemd/                     the user unit
scripts/delete-webhook.sh           webhook removal that does not leak the token
scripts/smoke.sh                    end-to-end run of the real binary against stubs
scripts/stub-telegram-api.py        the stub Bot API that smoke.sh drives
Makefile                            build / test / race / vet / fmt / install
```

Useful entry points when reading the code: `(*Bot).Claim` and `(*Bot).Handle` for
the gates, `(*Bot).runTurn` for locking and the turn lifecycle,
`(*Runner).Argv` for the Codex command lines, `(*Accumulator).Verdict` for how a
turn is judged, and `migrations/001_init.sql` for the schema.

### The smoke test

`go test` covers the packages, but it talks to an in-memory fake and so never
executes `cmd/bot/main.go`. This does — it builds the binary, runs it against a
stub Bot API and a fake Codex CLI, and checks the whole path:

```sh
scripts/smoke.sh          # add KEEP=1 to leave the temporary directories behind
```

It asserts the startup probes ran, the lock was taken, the webhook was cleared,
the turn succeeded, the exact argv and working directory reached Codex, the bot
token did **not** appear in the log or in the child's environment, `bot.db` was
created `0600`, a second instance was refused while the first was running, and
SIGTERM produced a clean shutdown. No token, network or Codex account required.
