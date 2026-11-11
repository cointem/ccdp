"""Exercise fragmented mouse input through a real PTY and the production TUI.
Usage: PYTHONPATH=<pyte-install> python3 pty_mouse.py /tmp/ccdp-tui.test /tmp/result.json
Uses the deterministic fixture; no model, credentials, or saved sessions.
"""
import codecs
import fcntl
import json
import os
import pty
import select
import struct
import subprocess
import sys
import termios
import time

import pyte


def run_fixture(binary, width, height):
    master, slave = pty.openpty()
    fcntl.ioctl(slave, termios.TIOCSWINSZ, struct.pack("HHHH", height, width, 0, 0))
    env = dict(os.environ, CCDP_TUI_PTY_FIXTURE="1", TERM="xterm-256color",
               CCDP_TUI_PTY_EXPECT_DRAFT="draft 中文")
    process = subprocess.Popen([binary, "-test.run", "^TestPTYRearchitectureFixture$"],
                               stdin=slave, stdout=slave, stderr=slave, env=env,
                               start_new_session=True)
    os.close(slave)
    screen = pyte.Screen(width, height)
    stream = pyte.Stream(screen)
    decoder = codecs.getincrementaldecoder("utf8")("replace")
    raw = bytearray()
    frames = {}

    def drain(seconds=0.3):
        end = time.monotonic() + seconds
        while time.monotonic() < end:
            if not select.select([master], [], [], 0.01)[0]:
                continue
            try:
                data = os.read(master, 65536)
            except OSError:
                break
            if not data:
                break
            raw.extend(data)
            stream.feed(decoder.decode(data))
            if b"\x1b[6n" in data:
                os.write(master, f"\x1b[{screen.cursor.y+1};{screen.cursor.x+1}R".encode())

    def frame(name):
        frames[name] = list(screen.display)
        return "\n".join(screen.display)

    try:
        drain(2)
        for _ in range(8):
            if "LIVE_RESULT_COMPLETE" in "\n".join(screen.display):
                break
            drain(0.5)
        assert "LIVE_RESULT_COMPLETE" in frame("initial"), f"fixture did not become ready at {width}x{height}"
        assert b"\x1b[?1002h" in raw, "mouse reporting was not enabled"
        thought_y = next(y for y, line in enumerate(screen.display) if "Thought" in line)
        for end in ("M", "m"):
            report = f"\x1b[<0;4;{thought_y+1}{end}".encode()
            os.write(master, report[:2])
            drain(0.02)
            os.write(master, report[2:])
            drain(0.02)
        drain()
        assert "[click to collapse]" in frame("split-click"), "split click did not open detail"
        os.write(master, b"\x1b")
        drain(0.02)
        os.write(master, b"[<65;4;4M")
        drain()
        assert "DETAIL_SENTINEL" in frame("split-detail-scroll"), "split wheel did not scroll detail"
        os.write(master, b"\x1b")
        drain()
        assert "[click to collapse]" not in frame("escape"), "standalone Escape did not close detail"

        literal = "[<65;62;14M"
        os.write(master, ("\x1b[200~" + literal + "\x1b[201~").encode())
        drain()
        assert literal in frame("literal-paste"), "literal paste was mistaken for mouse input"
        os.write(master, b"\x1b\x1b")
        drain()
        assert literal not in frame("double-escape"), "double Escape did not clear draft"

        os.write(master, "draft 中文".encode())
        drain()
        report = b"\x1b[<65;62;14M"
        for split in range(1, len(report)):
            os.write(master, report[:split])
            drain(0.02)
            os.write(master, report[split:])
            drain(0.02)
        os.write(master, report * 200)
        drain()
        view = frame("split-wheel-and-burst")
        assert "draft 中文" in view, "mouse input lost draft"
        assert "[<65;" not in view, "mouse report leaked into composer"

        # The fixture also checks the exact final draft, independent of layout.
        os.write(master, b"\x03")
        drain()
        os.write(master, b"\x03")
        drain()
        if "Exit ccdp?" in "\n".join(screen.display):
            os.write(master, b"y")
            drain()
        process.wait(timeout=4)
        assert process.returncode == 0, raw.decode("utf8", "replace")[-2000:]
        assert b"\x1b[?2004l" in raw and b"\x1b[?1006l" in raw, "terminal modes were not restored"
        return {"size": [width, height], "frames": frames, "passed": True}
    finally:
        with open(f"{sys.argv[2]}.{width}.debug.json", "w") as output:
            json.dump({"frames": frames, "raw": raw.decode("utf8", "replace")},
                      output, ensure_ascii=False, indent=2)
        if process.poll() is None:
            process.kill()
            process.wait()
        os.close(master)


results = [run_fixture(sys.argv[1], w, h) for w, h in [(80, 24), (40, 16), (120, 40), (200, 56)]]
with open(sys.argv[2], "w") as output:
    json.dump(results, output, ensure_ascii=False, indent=2)
print("PTY mouse passed at 80x24, 40x16, 120x40, 200x56: split click/wheel, bursts, literal paste, Escape, draft preservation, exit")
