# The Muse avatar

When Muse is what answers (`brain.mode` is `muse`), an Echo Show can draw Muse's character in
the middle of the screen for a turn: listening, thinking and talking as the turn does, with no words
beside it. The character is Meta's.
Meta's SDK says its Apache license does not cover it, so neither the character nor the code that
draws it is in this repository or in any image built from it. `make_sprites.py` makes the pictures
from your own copy of the SDK, for your own device.

## Make the sprite sheets

You need a C compiler, [uv](https://docs.astral.sh/uv/) and a checkout of
[muse-gadget-sdk](https://github.com/facebookincubator/muse-gadget-sdk).

```sh
uv run --with pillow tools/muse-avatar/make_sprites.py --sdk ~/src/muse-gadget-sdk
```

That builds the SDK's renderer with the SDK's own animation driver, plays four animations and writes
them to `build/muse-avatar/`, which git ignores:

| File | What it holds |
|---|---|
| `manifest.json` | the frame size, the frame rate, and each animation's frame count |
| `idle.png` | the character at rest, shown while an answer stays up |
| `listening.png` | shown while the device listens |
| `thinking.png` | shown while Muse works out its answer |
| `talking.png` | shown while the answer is spoken (the SDK calls it `speaking`) |

Each sheet is the animation's frames in rows of ten, 64 pixels square, the size the renderer draws
at. The screen scales a frame up by a whole number as it draws it: five on a Show 5, for 320 pixels,
and six on a Show 8. Frames are every second one of the renderer's 25 a second, so 12.5 a second.

## Put them on the device

```sh
scp -r build/muse-avatar root@<address>:/data/misc/techo5/
ssh root@<address> killall techo5
```

The daemon reads the set once as it starts and logs `Muse avatar loaded`. A set it cannot read is
logged as `no Muse avatar installed` with the reason, and turns are drawn as they are without one.
To take the character off again, delete `/data/misc/techo5/muse-avatar` and restart the daemon.

The Echo Spot's round screen and the Echo Dot do not draw the character.

## From videos, for a smoother character

`make_sprites.py` draws Meta's pixel character. `make_video_sprites.py` makes a set from four short
videos of a character on a plain white background instead, one each for idle, listening, thinking
and talking:

```sh
uv run --with numpy --with pillow --with opencv-python-headless tools/muse-avatar/make_video_sprites.py \
    --idle idle.mp4 --listening listening.mp4 --thinking thinking.mp4 --talking talking.mp4
```

It needs `ffmpeg`. It takes the white out from the frame's edges inward, keeps the character's edge
soft, folds the end of each clip over its start so the loop has no jump, and writes the same kind of
folder, to copy to the device the same way. The set is larger: at the default 432 pixels high and 12
frames a second, four six-second clips are about 14 MB on disk and 50 MB of the device's memory.

What makes a good video: the same framing in all four, a background that is one flat color, nothing
of the character leaving the picture, and three to six seconds each. The videos are yours to supply
and, like the character, are not in this repository.
