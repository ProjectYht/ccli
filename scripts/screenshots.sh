#!/usr/bin/env bash
# Renders the README screenshots from a synthetic home (scripts/demo), so no
# real session data ends up in them. Needs go, jq and freeze
# (go install github.com/charmbracelet/freeze@latest); ImageMagick, if
# present, shrinks the images.
#
#   scripts/screenshots.sh            # writes docs/images/*.png
set -euo pipefail

repo="$(cd "$(dirname "$0")/.." && pwd)"
out="$repo/docs/images"
work="$(mktemp -d)"
live=""
trap '[ -n "$live" ] && kill "$live" 2>/dev/null; rm -rf "$work"' EXIT

home="$work/home"
mkdir -p "$out"
go build -o "$work/ccli" "$repo"

# A process that stays alive, registered as the running session.
sleep 600 &
live=$!
go run "$repo/scripts/demo" -home "$home" -live-pid "$live" >/dev/null

ccli() {
	HOME="$home" XDG_DATA_HOME="$home/.local/share" XDG_CONFIG_HOME="$home/.config" \
		XDG_STATE_HOME="$home/.local/state" COLUMNS="${COLS:-112}" "$work/ccli" "$@"
}
ccli index >/dev/null

# Two status line snapshots, one per account, for ccli quota.
now=$(date +%s)
snap() { # snap <config dir> <5h %> <5h reset> <7d %> <7d reset>
	local tp
	tp=$(find "$1/projects" -maxdepth 2 -name '*.jsonl' | head -1)
	printf '{"session_id":"demo","transcript_path":"%s","model":{"id":"claude-opus-5-5"},"rate_limits":{"five_hour":{"used_percentage":%s,"resets_at":%s},"seven_day":{"used_percentage":%s,"resets_at":%s}}}' \
		"$tp" "$2" "$3" "$4" "$5" | ccli statusline -- cat >/dev/null
}
snap "$home/.claude" 31 $((now + 7800)) 58 $((now + 302400))
snap "$home/.claude-work" 74 $((now + 3000)) 41 $((now + 190000))

lead=$(ccli sessions --json -n 200 | jq -r '.sessions[] | select(.title == "Audit payment retries across services") | .id')
ahead=$(ccli diff default work --json | jq -r '[.[] | select(.state | test("ahead"))][0].session_id')

shot() { # shot <name> <ccli args...>
	local name=$1
	shift
	# The fake home lives in a temp dir; show it as a plain home path.
	ccli "$@" --color always | sed "s#$home#/Users/alex#g" >"$work/$name.ansi"
	freeze "$work/$name.ansi" --language ansi --window --output "$out/$name.png" \
		--padding 24,32 --margin 0 --border.radius 10 --font.size 14 --line-height 1.3 \
		--background "#16161e" >/dev/null
	# freeze renders at 4x; half that is still sharp on high-density screens.
	if command -v magick >/dev/null; then
		magick "$out/$name.png" -resize 50% -strip -colors 128 "$out/$name.png"
	fi
	echo "$out/$name.png"
}

shot stats stats
COLS=136 shot sessions sessions -n 16
shot show show "${lead:0:8}"
shot accounts accounts
shot quota quota
shot diff diff default work --project web-dashboard --all
shot copy copy "${ahead:0:8}" --from work --to default
