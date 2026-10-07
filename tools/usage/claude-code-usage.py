#!/usr/bin/env python3
"""Send a Claude Code plan's usage to a Show's Usage clock style.

Claude Code runs this as its status line command and hands it the session as JSON on stdin, which
for a Pro or Max plan carries rate_limits: how much of the five hour and the seven day allowance is
used, and when each starts over. This posts those to the device's /meters and prints nothing, so
the terminal is left as it was. See docs/usage-screen.md.

It reads ~/.config/heymuse/usage.json: {"url": "http://<device>:8181/meters", "key": "<meters key>"}.
"""
import json
import os
import sys
import time
import urllib.request

CONFIG = os.path.expanduser("~/.config/heymuse/usage.json")
LAST = os.path.expanduser("~/.config/heymuse/usage.last")
# A board that has not changed is sent again this often, so the device knows it is still current.
AGAIN = 300


def board(session):
    limits = session.get("rate_limits") or {}
    rows = []
    for label, name in (("Current", "five_hour"), ("Weekly", "seven_day")):
        window = limits.get(name) or {}
        if "used_percentage" in window:
            rows.append({"label": label, "percent": window["used_percentage"], "resets_at": int(window.get("resets_at") or 0)})
    if not rows:
        return None
    return {"title": "Usage", "note": (session.get("model") or {}).get("display_name", ""), "rows": rows}


def main():
    try:
        b = board(json.load(sys.stdin))
        with open(CONFIG) as f:
            to = json.load(f)
    except (OSError, ValueError):
        return
    if b is None:
        return
    body = json.dumps(b, sort_keys=True)
    try:
        with open(LAST) as f:
            if f.read() == body and time.time() - os.path.getmtime(LAST) < AGAIN:
                return
    except OSError:
        pass
    # The post is made by a child, so a device that is off or slow never holds Claude Code up.
    if os.fork():
        return
    os.setsid()
    try:
        req = urllib.request.Request(to["url"], data=body.encode(), method="POST",
                                     headers={"Authorization": "Bearer " + to["key"], "Content-Type": "application/json"})
        urllib.request.urlopen(req, timeout=4).close()
        with open(LAST, "w") as f:
            f.write(body)
    except Exception:
        pass
    os._exit(0)


if __name__ == "__main__":
    main()
