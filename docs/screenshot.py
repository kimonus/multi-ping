#!/usr/bin/env python3
"""Take a PNG screenshot of the real multi-ping terminal UI.

Runs the program in a pseudo-terminal, lets it ping for a while, optionally
sends keys, then draws the final screen with a monospaced font.

    docs/screenshot.py OUT.png COLSxROWS SECONDS [KEYS] -- [multi-ping args]

KEYS may use \\t, \\r and \\e (Escape); a trailing key sequence is given one
more second to take effect. Needs: pip install pyte pillow, DejaVu Sans Mono.
"""
import fcntl, os, pty, select, signal, struct, sys, termios, time

import pyte
from PIL import Image, ImageDraw, ImageFont

FONT = "/usr/share/fonts/truetype/dejavu/DejaVuSansMono%s.ttf"
SIZE, PAD = 22, 24
BG, FG = "#16181d", "#d7dae0"
COLORS = {
    "black": "#16181d", "red": "#e06c75", "green": "#98c379", "brown": "#e5c07b",
    "blue": "#61afef", "magenta": "#c678dd", "cyan": "#56b6c2", "white": "#d7dae0",
    "brightblack": "#6b7280", "brightred": "#ff6b6b", "brightgreen": "#7ee787",
    "brightbrown": "#f2cc60", "brightblue": "#79c0ff", "brightmagenta": "#d2a8ff",
    "brightcyan": "#56d4dd", "brightwhite": "#ffffff",
}


def run(cols, rows, seconds, keys, args):
    exe = os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "multi-ping")
    pid, fd = pty.fork()
    if pid == 0:
        os.environ["TERM"] = "xterm-256color"
        os.execv(exe, ["multi-ping"] + args)
    fcntl.ioctl(fd, termios.TIOCSWINSZ, struct.pack("HHHH", rows, cols, 0, 0))
    screen = pyte.Screen(cols, rows)
    stream = pyte.ByteStream(screen)
    answered = False

    def pump(secs):
        nonlocal answered
        until = time.time() + secs
        while time.time() < until:
            if not select.select([fd], [], [], 0.1)[0]:
                continue
            data = os.read(fd, 65536)
            if not answered and b"\x1b[6n" in data:
                # Answer the colour and cursor queries like a dark terminal would.
                os.write(fd, b"\x1b]11;rgb:1616/1818/1d1d\x1b\\\x1b[1;1R")
                answered = True
            stream.feed(data)

    pump(seconds)
    if keys:
        for k in keys:
            os.write(fd, k)
            pump(0.3)
        pump(1)
    os.kill(pid, signal.SIGKILL)
    os.waitpid(pid, 0)
    return screen


def color(name, default):
    if name == "default":
        return default
    return COLORS.get(name) or "#" + name


def draw(screen, out):
    regular, bold = ImageFont.truetype(FONT % "", SIZE), ImageFont.truetype(FONT % "-Bold", SIZE)
    cw = round(regular.getlength("M"))
    ch = sum(regular.getmetrics())
    img = Image.new("RGB", (screen.columns * cw + 2 * PAD, screen.lines * ch + 2 * PAD), BG)
    d = ImageDraw.Draw(img)
    for y in range(screen.lines):
        for x in range(screen.columns):
            c = screen.buffer[y][x]
            fg, bg = color(c.fg, FG), color(c.bg, BG)
            if c.reverse:
                fg, bg = bg, fg
            px, py = PAD + x * cw, PAD + y * ch
            if bg != BG:
                d.rectangle([px, py, px + cw - 1, py + ch - 1], fill=bg)
            if c.data == "│":
                # Antialiasing leaves a gap between stacked cells; overlap them.
                for dy in (-1, 1):
                    d.text((px, py + dy), c.data, font=regular, fill=fg)
            if c.data.strip():
                d.text((px, py), c.data, font=bold if c.bold else regular, fill=fg)
    img.save(out, optimize=True)


def main():
    argv = sys.argv[1:]
    if "--" not in argv or argv.index("--") not in (3, 4):
        sys.exit(__doc__)
    split = argv.index("--")
    out, size, seconds = argv[0], argv[1], float(argv[2])
    keys = argv[3] if split == 4 else ""
    cols, rows = map(int, size.lower().split("x"))
    seq, i = [], 0
    while i < len(keys):  # split into single keys, decoding the escapes
        if keys[i] == "\\" and i + 1 < len(keys):
            seq.append({"t": b"\t", "r": b"\r", "e": b"\x1b"}.get(keys[i + 1], keys[i + 1].encode()))
            i += 2
        else:
            seq.append(keys[i].encode())
            i += 1
    draw(run(cols, rows, seconds, seq, argv[split + 1:]), out)


if __name__ == "__main__":
    main()
