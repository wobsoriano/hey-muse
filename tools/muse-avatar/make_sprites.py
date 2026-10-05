#!/usr/bin/env python3
"""Make the Muse avatar's sprite sheets for the Show's turn screen.

    uv run --with pillow tools/muse-avatar/make_sprites.py --sdk ~/src/muse-gadget-sdk

Meta's Muse gadget SDK draws its default character with a C renderer. This
builds that renderer on this computer with the SDK's own animation driver,
plays the animations the screen needs, and packs the frames into one PNG sheet
per animation plus the manifest echod reads (echod/internal/lib/avatar).

The character is Meta's and the SDK's Apache license does not cover it, so
nothing of it is kept in this repository: the renderer is read from your own
SDK checkout, and the sheets go to build/, which git ignores. They are for your
own device, not for publishing.
"""

import argparse
import json
import pathlib
import subprocess
import sys
import tempfile

from PIL import Image

ROOT = pathlib.Path(__file__).resolve().parent.parent.parent
OUT = ROOT / "build" / "muse-avatar"

# The renderer's own resolution. The sheets keep it: the screen scales each
# frame up by a whole number as it draws (5 on a Show 5, for 320 pixels), which
# loses nothing and keeps the frames in memory 25 times smaller.
CELL = 64
DRIVER_FPS = 25
FRAME_STEP = 2      # every second frame: 12.5 a second is what the screen draws
COLUMNS = 10

# What the screen calls each animation -> what the SDK's driver calls it.
ANIMATIONS = {"idle": "idle", "listening": "listening", "thinking": "thinking",
              "talking": "speaking"}


def render_frames(esp32, work):
    """Build the SDK's driver against its default avatar and dump every frame."""
    for needed in ("tools/muse/anim.c", "avatar/muse_pixel.c", "components/muse/muse_pixel.h"):
        if not (esp32 / needed).is_file():
            sys.exit("%s is not a muse-gadget-sdk checkout: no esp32/%s" % (esp32.parent, needed))
    exe = work / "muse_anim"
    try:
        subprocess.run(
            ["cc", "-O2", "-I", "components/muse", "tools/muse/anim.c", "avatar/muse_pixel.c",
             "-lm", "-o", str(exe)],
            cwd=esp32, check=True, capture_output=True, text=True)
    except subprocess.CalledProcessError as error:
        sys.exit((error.stdout or "") + (error.stderr or "") + "\nthe renderer does not build")
    frames = work / "frames"
    frames.mkdir()
    subprocess.run([str(exe), str(frames)], check=True)
    return frames


def grid_shade(big):
    """How bright the driver draws the last row and column of each cell, 0 to 1.

    The driver draws every cell several pixels wide with its last pixel dimmed,
    which is the grid the character is seen through on a gadget's own display.
    The screen redraws that grid itself, so the sheets carry only how dark it is.
    """
    step = big.width // CELL
    lit = edge = 0
    for cy in range(CELL):
        for cx in range(CELL):
            lit += sum(big.getpixel((cx * step + step // 2, cy * step + step // 2)))
            edge += sum(big.getpixel((cx * step + step - 1, cy * step + step // 2)))
    return round(edge / lit, 2) if lit else 1.0


def cut_out(frame):
    """The frame with the ground around the character made clear.

    The renderer fills what it does not draw with one flat color and draws the
    character with others, so every pixel of that color is ground: around the
    character, between the dots of its glow, and in the gaps under an arm or
    between its feet, which a flood from the edges would leave as dark holes.
    """
    ground = frame.getpixel((0, 0))
    cut = frame.convert("RGBA")
    cut.putdata([(0, 0, 0, 0) if p[:3] == ground else p for p in cut.get_flattened_data()])
    return cut


def sheet_of(frames):
    """The frames side by side, as a paletted picture whose first color is clear."""
    rows = (len(frames) + COLUMNS - 1) // COLUMNS
    rgba = Image.new("RGBA", (COLUMNS * CELL, rows * CELL), (0, 0, 0, 0))
    for index, frame in enumerate(frames):
        rgba.paste(frame, ((index % COLUMNS) * CELL, (index // COLUMNS) * CELL))
    colors = sorted({p[:3] for p in rgba.get_flattened_data() if p[3]})
    if len(colors) > 255:
        sys.exit("a sheet has %d colors, more than a palette holds" % len(colors))
    # Exact colors, no quantizing: the renderer draws from a palette of its own.
    index_of = {c: i + 1 for i, c in enumerate(colors)}
    sheet = Image.new("P", rgba.size, 0)
    sheet.putpalette([0, 0, 0] + [v for c in colors for v in c])
    sheet.putdata([index_of[p[:3]] if p[3] else 0 for p in rgba.get_flattened_data()])
    return sheet


def main():
    parser = argparse.ArgumentParser(description=__doc__.split("\n")[0])
    parser.add_argument("--sdk", required=True, help="your muse-gadget-sdk checkout")
    parser.add_argument("--out", default=str(OUT), help="where the sheets go (default: build/muse-avatar)")
    args = parser.parse_args()

    out = pathlib.Path(args.out).expanduser()
    out.mkdir(parents=True, exist_ok=True)
    manifest = {"cell": CELL, "fps": DRIVER_FPS / FRAME_STEP, "animations": {}}
    with tempfile.TemporaryDirectory() as tmp:
        frames_dir = render_frames(pathlib.Path(args.sdk).expanduser().resolve() / "esp32", pathlib.Path(tmp))
        for name, theirs in ANIMATIONS.items():
            paths = sorted((frames_dir / theirs).glob("*.ppm"))[::FRAME_STEP]
            if not paths:
                sys.exit("the SDK's driver drew no %r animation" % theirs)
            big = [Image.open(p).convert("RGB") for p in paths]
            manifest.setdefault("grid", grid_shade(big[0]))
            # Nearest-neighbor reads the middle pixel of each cell, which is its true color.
            frames = [cut_out(b.resize((CELL, CELL), Image.Resampling.NEAREST)) for b in big]
            target = out / (name + ".png")
            sheet_of(frames).save(target, optimize=True, transparency=0)
            manifest["animations"][name] = {"frames": len(frames), "columns": COLUMNS, "loop": True}
            print("%-10s %3d frames  %5.1f KB" % (name, len(frames), target.stat().st_size / 1024))
    (out / "manifest.json").write_text(json.dumps(manifest, indent=1) + "\n")
    files = [out / "manifest.json"] + [out / (n + ".png") for n in ANIMATIONS]
    print("total %.1f KB in %s" % (sum(p.stat().st_size for p in files) / 1024, out))


if __name__ == "__main__":
    main()
