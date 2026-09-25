#!/usr/bin/env bash
# delete-webhook.sh — remove a Telegram webhook without putting the bot token in
# shell history, in a process's argv, or in a file.
#
# Why this needs a script at all: the token is part of the request URL, so
#
#   curl "https://api.telegram.org/bot<TOKEN>/deleteWebhook"
#
# shows up in your shell history and in `ps` output for as long as curl runs.
# This script reads the token from the bot's own environment file and hands the
# URL to curl through a config file on a pipe, so it never appears in argv.
#
# Usage:
#   scripts/delete-webhook.sh [-f /path/to/bot.env] [-d]
#
#   -f  environment file to read TELEGRAM_BOT_TOKEN from
#       (default: $HOME/.config/codex-telegram-bot/bot.env)
#   -d  also drop pending updates. OFF by default, because dropping them throws
#       away every message sent while the bot was down.
#
# A webhook and a long-polling bot are mutually exclusive: while a webhook is
# configured, getUpdates answers 409 and the bot looks healthy while receiving
# nothing. The bot itself deletes the webhook at startup unless
# BOT_DELETE_WEBHOOK=false; this script is for doing it by hand, and for checking
# the current state.
set -euo pipefail

env_file="${HOME}/.config/codex-telegram-bot/bot.env"
drop_pending=false

while getopts ':f:dh' opt; do
	case "$opt" in
	f) env_file="$OPTARG" ;;
	d) drop_pending=true ;;
	h)
		sed -n '2,30p' "$0" | sed 's/^# \{0,1\}//'
		exit 0
		;;
	*)
		echo "delete-webhook.sh: unknown option -$OPTARG (try -h)" >&2
		exit 2
		;;
	esac
done

if [[ ! -r "$env_file" ]]; then
	echo "delete-webhook.sh: cannot read $env_file" >&2
	exit 1
fi

# Read only the token, without exporting the whole file into this process's
# environment and without echoing it.
token="$(sed -n 's/^TELEGRAM_BOT_TOKEN=//p' "$env_file" | head -n 1)"
token="${token%\"}"
token="${token#\"}"
if [[ -z "$token" ]]; then
	echo "delete-webhook.sh: TELEGRAM_BOT_TOKEN not found in $env_file" >&2
	exit 1
fi

base="$(sed -n 's/^BOT_TELEGRAM_API_BASE=//p' "$env_file" | head -n 1)"
base="${base:-https://api.telegram.org}"

# Show the state first, so you can see whether there was anything to delete.
echo "--- getWebhookInfo ---"
printf 'url = "%s/bot%s/getWebhookInfo"\n' "$base" "$token" |
	curl --silent --show-error --config - |
	sed "s/${token}/[redacted]/g"

echo
echo "--- deleteWebhook (drop_pending_updates=${drop_pending}) ---"
printf 'url = "%s/bot%s/deleteWebhook?drop_pending_updates=%s"\nrequest = "POST"\n' \
	"$base" "$token" "$drop_pending" |
	curl --silent --show-error --config - |
	sed "s/${token}/[redacted]/g"

echo
echo "--- getWebhookInfo (after) ---"
printf 'url = "%s/bot%s/getWebhookInfo"\n' "$base" "$token" |
	curl --silent --show-error --config - |
	sed "s/${token}/[redacted]/g"
echo
