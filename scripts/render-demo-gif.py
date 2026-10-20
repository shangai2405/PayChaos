#!/usr/bin/env python3
"""Render the output of scripts/demo.sh as an animated terminal GIF.

    ./scripts/demo.sh | python3 scripts/render-demo-gif.py docs/demo.gif

The GIF is drawn from the real output of a run, condensed to the lines that
matter (headers, results, finding counts, one example finding per client).
Requires Pillow and the DejaVu Sans Mono font.
"""
import re
import sys
import textwrap

from PIL import Image, ImageDraw, ImageFont

FONT = "/usr/share/fonts/truetype/dejavu/DejaVuSansMono.ttf"
BOLD = "/usr/share/fonts/truetype/dejavu/DejaVuSansMono-Bold.ttf"
SIZE, LINE_H, PAD, COLS = 15, 22, 18, 92
BG, FG, DIM = (22, 24, 29), (220, 223, 228), (130, 136, 148)
CYAN, RED, GREEN, YELLOW = (97, 175, 239), (239, 108, 108), (120, 200, 120), (229, 192, 123)


def condense(lines):
    out, shown_example = [("$ make demo", FG, True)], False
    for raw in lines:
        line = raw.rstrip()
        if line.startswith("== "):
            out += [("", FG, False), (line.strip("= ").strip(), CYAN, True)]
            shown_example = False
        elif "checkouts reported success" in line:
            out.append(("  " + line, DIM, False))
        elif "upstream created" in line:
            m = re.search(r"created (\d+) PaymentIntents for (\d+)", line)
            bad = m and m.group(1) != m.group(2)
            out.append(("  " + line.strip(), RED if bad else GREEN, True))
        elif line.startswith("Findings:"):
            out.append(("  " + line, RED if not line.startswith("Findings: 0 high") else GREEN, False))
        elif line.startswith("    request #") and not shown_example:
            msg = re.sub(r"pi_[0-9a-f]{24}", lambda m: m.group(0)[:9] + "…", line.strip())
            for i, part in enumerate(textwrap.wrap(msg, COLS - 6, break_on_hyphens=False)):
                out.append(("    " + part, DIM if i else YELLOW, False))
            shown_example = True
    return out


def render(rows, path):
    font, bold = ImageFont.truetype(FONT, SIZE), ImageFont.truetype(BOLD, SIZE)
    w = PAD * 2 + int(font.getlength("M") * COLS)
    h = PAD * 2 + LINE_H * len(rows)
    frames, durations = [], []
    for n in range(1, len(rows) + 1):
        img = Image.new("RGB", (w, h), BG)
        d = ImageDraw.Draw(img)
        for i, (text, color, strong) in enumerate(rows[:n]):
            d.text((PAD, PAD + i * LINE_H), text, fill=color, font=bold if strong else font)
        frames.append(img)
        durations.append(450 if rows[n - 1][1] in (RED, GREEN) and rows[n - 1][2] else 160)
    durations[-1] = 4000
    frames[0].save(path, save_all=True, append_images=frames[1:], duration=durations, loop=0, optimize=True)


if __name__ == "__main__":
    if len(sys.argv) != 2:
        sys.exit("usage: demo.sh | render-demo-gif.py OUT.gif")
    render(condense(sys.stdin.read().splitlines()), sys.argv[1])
