#!/usr/bin/env python3
"""Make a Muse avatar sprite set for a TECHO5 Show from short videos of the character on a plain white
background: one video each for idle, listening, thinking and talking.

    uv run --with numpy --with pillow --with opencv-python-headless tools/muse-avatar/make_video_sprites.py \\
        --idle idle.mp4 --listening listening.mp4 --thinking thinking.mp4 --talking talking.mp4

Needs ffmpeg. The sheets go to build/muse-avatar (--out), in the form echod's lib/avatar reads: one
paletted PNG per animation with the white taken out, soft at the character's edge, and manifest.json.
Copy the folder to /data/misc/techo5/muse-avatar on the device. The videos, like the character, are
not part of this repository.

The white is found from the frame's edges inward, so white inside the character (the shine in an eye)
stays. At the edge the fur is already mixed with the white behind it, so the white is taken back out of
the color as well as out of the alpha: without that the character carries a pale rim onto a dark screen.
"""
import argparse
import json
import pathlib
import subprocess
import sys
import tempfile

import cv2
import numpy as np
from PIL import Image

ROOT = pathlib.Path(__file__).resolve().parents[2]
ANIMATIONS = ("idle", "listening", "thinking", "talking")
COLUMNS = 10


def frames(video, fps, work):
    """The video's frames at fps, as RGB arrays."""
    out = work / video.stem
    out.mkdir()
    subprocess.run(["ffmpeg", "-v", "error", "-i", str(video), "-vf", "fps=%g" % fps, str(out / "%04d.png")], check=True)
    return [np.asarray(Image.open(p).convert("RGB")) for p in sorted(out.glob("*.png"))]


def cut_out(rgb, edge_fade):
    """The frame as RGBA with the white behind the character gone."""
    f = rgb.astype(np.float32)
    lo, hi = f.min(axis=2), f.max(axis=2)
    whitish = ((lo >= 236) & (hi - lo <= 16)).astype(np.uint8)
    # Only the white that reaches the frame's edge is the background.
    n, labels = cv2.connectedComponents(whitish, connectivity=4)
    border = np.unique(np.concatenate([labels[0], labels[-1], labels[:, 0], labels[:, -1]]))
    background = np.isin(labels, border[border != 0]) & (whitish == 1)
    solid = (~background).astype(np.uint8)
    # Specks of not-quite-white left in the background are not the character.
    n, labels, stats, _ = cv2.connectedComponentsWithStats(solid, connectivity=8)
    if n > 1:
        keep = 1 + int(np.argmax(stats[1:, cv2.CC_STAT_AREA]))
        big = stats[:, cv2.CC_STAT_AREA] >= 0.02 * stats[keep, cv2.CC_STAT_AREA]
        big[0] = False
        solid = big[labels].astype(np.uint8)

    inside = cv2.distanceTransform(solid, cv2.DIST_L2, 3)
    # How white the fur is where it is certainly fur, spread out to the edge: what "all character"
    # looks like next to each edge pixel.
    deep = (inside > 6).astype(np.float32)
    near = cv2.GaussianBlur(lo * deep, (0, 0), 9) / np.maximum(cv2.GaussianBlur(deep, (0, 0), 9), 1e-3)
    near = np.where(cv2.GaussianBlur(deep, (0, 0), 9) > 0.02, near, 200.0)
    # At the edge, a pixel is as much character as it is less white than white.
    by_color = np.clip((250.0 - lo) / np.maximum(250.0 - near, 12.0), 0, 1)
    alpha = np.where(inside > 5, 1.0, by_color) * solid
    alpha = np.minimum(alpha, cv2.GaussianBlur(solid.astype(np.float32), (0, 0), 1.0) * 2.0)
    alpha = np.clip(cv2.GaussianBlur(alpha, (0, 0), 0.6), 0, 1)
    alpha[inside > 5] = 1.0

    # An arm that leaves the picture ends in a straight cut; fade it out instead.
    h, w = alpha.shape
    ramp = np.clip(np.minimum(np.arange(w), w - 1 - np.arange(w)) / float(edge_fade), 0, 1)
    alpha *= ramp[None, :]

    a = alpha[..., None]
    color = np.where(a > 0.04, (f - (1 - a) * 255.0) / np.maximum(a, 0.04), f)
    out = np.dstack([np.clip(color, 0, 255), alpha * 255.0]).astype(np.uint8)
    out[out[..., 3] == 0] = 0
    return out


def loop(frames_rgba, blend):
    """The frames as a loop with no jump: the last blend frames are folded over the first."""
    n = len(frames_rgba)
    if blend <= 0 or n <= 2 * blend:
        return frames_rgba
    out = []
    for j in range(n - blend):
        if j < blend:
            t = j / float(blend)
            a, b = frames_rgba[n - blend + j].astype(np.float32), frames_rgba[j].astype(np.float32)
            # Blend premultiplied, so clear pixels do not drag their black in.
            pa, pb = a[..., 3:] / 255.0, b[..., 3:] / 255.0
            alpha = (1 - t) * pa + t * pb
            color = ((1 - t) * a[..., :3] * pa + t * b[..., :3] * pb) / np.maximum(alpha, 1e-3)
            out.append(np.dstack([np.clip(color, 0, 255), alpha * 255.0]).astype(np.uint8))
        else:
            out.append(frames_rgba[j])
    return out


def fit(rgba, cell):
    """The frame scaled to the cell's height and centered in a square cell."""
    h, w = rgba.shape[:2]
    nw = max(1, round(w * cell / h))
    img = Image.fromarray(rgba, "RGBA").resize((nw, cell), Image.LANCZOS)
    square = Image.new("RGBA", (cell, cell), (0, 0, 0, 0))
    if nw > cell:
        img = img.crop(((nw - cell) // 2, 0, (nw - cell) // 2 + cell, cell))
        nw = cell
    square.paste(img, ((cell - nw) // 2, 0))
    return square


# How the 256 colors of a sheet are shared out. Nearly all of them go to the character itself, where
# banding would show on its cheeks and fur; its soft edge is one color of fur at a few strengths, so
# a few base colors at EDGE_LEVELS strengths each are plenty there. Left to a general quantizer, the
# edge's many alphas took 224 of the 256 and the face was drawn in what was left.
EDGE_COLORS = 3
EDGE_LEVELS = 15
SOLID_COLORS = 256 - 1 - EDGE_COLORS * EDGE_LEVELS


def sheet(cells, cell):
    """The cells as one paletted sheet, COLUMNS across, the palette carrying each color's alpha."""
    rows = -(-len(cells) // COLUMNS)
    big = Image.new("RGBA", (COLUMNS * cell, rows * cell), (0, 0, 0, 0))
    for i, c in enumerate(cells):
        big.paste(c, ((i % COLUMNS) * cell, (i // COLUMNS) * cell))
    px = np.asarray(big)
    rgb, alpha = px[..., :3], px[..., 3]
    solid, clear = alpha >= 250, alpha < 8
    edge = ~solid & ~clear

    def palette_of(pixels, colors):
        sample = pixels[:: max(1, len(pixels) // 400000)]
        q = Image.fromarray(np.ascontiguousarray(sample).reshape(-1, 1, 3), "RGB").quantize(colors, method=Image.Quantize.MEDIANCUT)
        return np.asarray(q.getpalette()[: 3 * colors], dtype=np.uint8).reshape(-1, 3)

    def nearest(pixels, palette):
        out = np.empty(len(pixels), dtype=np.int32)
        pal = palette.astype(np.int32)
        for at in range(0, len(pixels), 200000):
            d = pixels[at:at + 200000, None, :].astype(np.int32) - pal[None]
            out[at:at + 200000] = np.argmin((d * d).sum(axis=2), axis=1)
        return out

    solid_pal = palette_of(rgb[solid], SOLID_COLORS)
    edge_pal = palette_of(rgb[edge], EDGE_COLORS) if edge.any() else np.zeros((EDGE_COLORS, 3), np.uint8)
    solid_pal = np.vstack([solid_pal, np.zeros((SOLID_COLORS - len(solid_pal), 3), np.uint8)])
    edge_pal = np.vstack([edge_pal, np.zeros((EDGE_COLORS - len(edge_pal), 3), np.uint8)])

    index = np.zeros(alpha.shape, dtype=np.uint8)
    palette, alphas = [(0, 0, 0)], [0]
    for c in edge_pal:
        for level in range(EDGE_LEVELS):
            palette.append(tuple(int(v) for v in c))
            alphas.append(round(255 * (level + 1) / (EDGE_LEVELS + 1)))
    first_solid = len(palette)
    for c in solid_pal:
        palette.append(tuple(int(v) for v in c))
        alphas.append(255)

    if edge.any():
        level = np.clip(np.rint(alpha[edge].astype(np.float32) * (EDGE_LEVELS + 1) / 255.0) - 1, 0, EDGE_LEVELS - 1)
        index[edge] = 1 + nearest(rgb[edge], edge_pal) * EDGE_LEVELS + level.astype(np.int32)
    index[solid] = first_solid + nearest(rgb[solid], solid_pal)

    out = Image.fromarray(index, "P")
    out.putpalette([v for c in palette for v in c])
    out.info["transparency"] = bytes(alphas)
    return out


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    for name in ANIMATIONS:
        ap.add_argument("--" + name, required=True, help="the %s video" % name)
    ap.add_argument("--out", default=str(ROOT / "build" / "muse-avatar"), help="where the sheets go")
    ap.add_argument("--cell", type=int, default=432, help="the character's height on the screen, in pixels")
    ap.add_argument("--fps", type=float, default=12, help="frames a second")
    ap.add_argument("--blend", type=int, default=6, help="frames folded over to close the loop; 0 for none")
    ap.add_argument("--edge-fade", type=int, default=20, help="pixels over which a cut-off limb fades at the sides")
    ap.add_argument("--preview", help="also write one frame of each animation here, on a dark ground")
    a = ap.parse_args()

    out = pathlib.Path(a.out)
    out.mkdir(parents=True, exist_ok=True)
    manifest = {"cell": a.cell, "fps": a.fps, "animations": {}}
    with tempfile.TemporaryDirectory() as tmp:
        for name in ANIMATIONS:
            video = pathlib.Path(getattr(a, name)).expanduser()
            if not video.is_file():
                sys.exit("no video at %s" % video)
            cut = [cut_out(f, a.edge_fade) for f in frames(video, a.fps, pathlib.Path(tmp))]
            cells = [fit(f, a.cell) for f in loop(cut, a.blend)]
            made = sheet(cells, a.cell)
            made.save(out / (name + ".png"), optimize=True, transparency=made.info["transparency"])
            manifest["animations"][name] = {"frames": len(cells), "columns": COLUMNS, "loop": True}
            print("%-10s %3d frames  %6.1f KB" % (name, len(cells), (out / (name + ".png")).stat().st_size / 1024))
            if a.preview:
                pathlib.Path(a.preview).mkdir(parents=True, exist_ok=True)
                ground = Image.new("RGBA", (a.cell, a.cell), (0x0e, 0x1a, 0x18, 255))
                ground.alpha_composite(cells[len(cells) // 3])
                ground.convert("RGB").save(pathlib.Path(a.preview) / (name + ".png"))
    (out / "manifest.json").write_text(json.dumps(manifest, indent=1) + "\n")
    decoded = sum(-(-v["frames"] // COLUMNS) * COLUMNS * a.cell * a.cell for v in manifest["animations"].values())
    print("decoded on the device: %.1f MB" % (decoded / 1048576))


if __name__ == "__main__":
    main()
