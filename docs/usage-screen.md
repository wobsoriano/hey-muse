# The Usage screen

A Show can draw a small board of meters in place of the clock: a title, and for each meter its share
of a hundred, a bar, and how long until it starts over. Something else on your network sends the
numbers. `tools/usage` sends a Claude Code plan's usage, which is what this was made for, but the
device does not know or mind what the numbers measure.

It is a clock style, named **Usage**, on the Show 5 and Show 8. The Spot and the Dot do not draw it.

**Tested** on a Show 5 2nd gen, fed by Claude Code on a Mac. Nothing else was tried.

## 1. Give the device a key

The device takes a board only from a sender that shows its key, and serves nothing until it has one.
Turn **SSH** on, then make a key and put it on the device:

```sh
KEY=$(openssl rand -hex 24)
printf '%s\n' "$KEY" | ssh root@<address> 'umask 077; cat > /data/misc/techo5/meters.key'
```

Restart the device, or the daemon with `ssh root@<address> killall techo5`, so the web port opens.
To turn this off again, delete the file and restart.

## 2. Choose the style

Swipe left or right across the clock until **Usage** shows, or pick it under Settings, Display,
Clock style. Until a board arrives it says "Waiting for numbers".

## 3. Send Claude Code's usage

Claude Code hands its [status line](https://code.claude.com/docs/en/statusline) command your plan's
usage: the five hour and the seven day allowance, and when each starts over. It does so for claude.ai
Pro and Max plans, after the first answer in a session. `tools/usage/claude-code-usage.py` sends those
on and prints nothing, so your terminal stays as it was.

```sh
mkdir -p ~/.config/heymuse && chmod 700 ~/.config/heymuse
cp tools/usage/claude-code-usage.py ~/.config/heymuse/
printf '{"url": "http://<address>:8181/meters", "key": "%s"}\n' "$KEY" > ~/.config/heymuse/usage.json
chmod 600 ~/.config/heymuse/usage.json
```

Then add this to `~/.claude/settings.json`:

```json
"statusLine": {
  "type": "command",
  "command": "/usr/bin/python3 ~/.config/heymuse/claude-code-usage.py",
  "refreshInterval": 60
}
```

With a status line command set, Claude Code hides most of its footer's keyboard hints. If you have a
status line of your own, call this script from it with the same input instead.

What to expect:

- The numbers move only while a Claude Code session is open on that computer. After 15 minutes with
  no board, the foot of the screen says how old the numbers are.
- "Resets in" counts down on the device by itself. A meter whose time has passed is drawn empty.
- Claude Code leaves an allowance out while nothing of it is used. The script sends that as 0%, with
  no time to start over, so the row stays on the screen.
- Only the two allowances Claude Code hands over are shown. A per-model weekly allowance is not among
  them.

## A mark beside the title

The device draws a picture at the top left if there is a PNG at `/data/misc/techo5/meters-mark.png`,
1024 pixels a side at most. The software carries none. Restart the daemon after putting one there.

## Sending your own numbers

Post JSON to `http://<address>:8181/meters` with the header `Authorization: Bearer <key>`:

```json
{
  "title": "Usage",
  "note": "a line of words for the foot",
  "rows": [
    {"label": "Current", "percent": 21, "resets_at": 1790000000},
    {"label": "Weekly", "percent": 5}
  ]
}
```

`resets_at` is Unix seconds, and may be left out. Up to four rows are drawn. The device answers 204
when it has taken the board, and 403 to a wrong key.

The key travels over your network in the clear, as the rest of the device's web port does. It lets
its holder change what this one screen shows and nothing else.
