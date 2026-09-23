"""Drive the compiled Go fixture in a real PTY and parse frames with pyte.
Usage: PYTHONPATH=<pyte-install> python3 pty_smoke.py /tmp/ccdp-tui.test /tmp/result.json
No model, credentials, or persisted sessions are used.
"""
import codecs
import fcntl
import json
import os
import pty
import re
import select
import signal
import struct
import subprocess
import sys
import termios
import time

import pyte

def run_fixture(binary, width, height):
    master, slave = pty.openpty()
    fcntl.ioctl(slave, termios.TIOCSWINSZ, struct.pack("HHHH", height, width, 0, 0))
    env = dict(os.environ, CCDP_TUI_PTY_FIXTURE="1", TERM="xterm-256color")
    env.pop("NO_COLOR", None)
    process = subprocess.Popen([binary, "-test.run", "^TestPTYRearchitectureFixture$"],
                               stdin=slave, stdout=slave, stderr=slave, env=env, start_new_session=True)
    os.close(slave)
    screen = pyte.HistoryScreen(width, height, history=10000)
    stream = pyte.Stream(screen)
    decoder = codecs.getincrementaldecoder("utf8")("replace")
    raw = bytearray()
    frames = {}
    def drain(seconds=0.4):
        end = time.monotonic() + seconds
        while time.monotonic() < end:
            if not select.select([master], [], [], 0.03)[0]:
                continue
            try:
                data = os.read(master, 65536)
            except OSError:
                break
            if not data:
                break
            raw.extend(data)
            stream.feed(decoder.decode(data))
            if b"[6n" in data:
                os.write(master, f"[{screen.cursor.y+1};{screen.cursor.x+1}R".encode())
    def frame(name):
        frames[name] = list(screen.display)
        return "\n".join(screen.display)
    def assert_composer_background(lines=1):
        row = next(y for y, line in enumerate(screen.display)
                   if "Ask CCDP" in line or "draft 中文" in line)
        expected = screen.buffer[row-1][0].bg
        assert expected != "default", "composer has no background"
        for y in range(row-1, row+lines+1):
            for x in range(screen.columns):
                cell = screen.buffer[y][x]
                # Wide-character continuation cells do not carry attributes in pyte.
                if cell.data == "":
                    continue
                assert cell.bg == expected, (f"composer background gap at {x},{y}: "
                                             f"{cell.bg} != {expected}")
    try:
        drain(2)
        for _ in range(8):
            if "LIVE_RESULT_COMPLETE" in "\n".join(screen.display):
                break
            drain(0.5)
        before = frame("initial")
        assert "LIVE_RESULT_COMPLETE" in before, "completed watch update missing from visible viewport"
        assert "LIVE_STREAM" not in before, "stale stream remains visible"
        def footer_row():
            return next(y for y, line in enumerate(screen.display) if "enter send" in line)
        assert b"\x1b[?1002h" in raw, "mouse reporting is not enabled"
        stable_details = list(screen.display)
        thought_y = next(y for y,line in enumerate(screen.display) if "Thought" in line)
        os.write(master, f"\x1b[<0;4;{thought_y+1}M\x1b[<0;4;{thought_y+1}m".encode())
        drain()
        assert "[click to collapse]" in frame("thought-detail"), "thought click did not open overlay"
        assert "[click to collapse]" in screen.display[thought_y], "detail moved away from clicked record"
        os.write(master,b"\x1b[<65;4;4M")
        drain()
        assert "DETAIL_SENTINEL" in frame("thought-scrolled"), "detail scroll did not show original content"
        os.write(master,b"\x1b")
        drain()
        assert list(screen.display)==stable_details, "closing thought moved background"
        # Footer clicks target metrics, and the context card overlays history.
        stable = list(screen.display)
        metric_y = next(y for y, line in enumerate(screen.display) if "200k" in line)
        metric_x = screen.display[metric_y].index("200k")
        os.write(master, f"\x1b[<0;{metric_x+1};{metric_y+1}M\x1b[<0;{metric_x+1};{metric_y+1}m".encode())
        drain()
        assert "Context Window" in frame("context-overlay"), "context metric click did not open card"
        top_border = next(line for line in screen.display if "╭" in line and "╮" in line)
        left = top_border.index("╭")
        card_width = top_border.index("╮") - left + 1
        metric = re.search(r"(?:\d+% \()?[\d.k]+/200k", stable[metric_y])
        assert metric is not None, "context metric missing"
        expected_left = min(metric.start(), width-card_width)
        assert left == expected_left, f"context card x={left}, expected {expected_left}"
        for line in screen.display:
            if "│" in line and line.count("│") == 2:
                assert line.index("│") == left, "card rows misaligned over short history"
        assert screen.display[metric_y] == stable[metric_y], "context card moved footer"
        composer_y = next(y for y,line in enumerate(stable) if "Ask CCDP" in line)
        assert screen.display[composer_y:] == stable[composer_y:], "context card moved composer"
        os.write(master, b"\x1b")
        drain()
        assert list(screen.display) == stable, "closing context card moved history"
        for title, result in [("SECOND_AGENT","CHILD_TWO_VIEW"),("FIRST_AGENT","CHILD_ONE_VIEW")]:
            row = next(y for y,line in enumerate(screen.display) if title in line)
            os.write(master, f"\x1b[<0;8;{row+1}M\x1b[<0;8;{row+1}m".encode())
            drain(0.7)
            assert result in frame(title), "mouse opened wrong child or no child"
            os.write(master,b"\x1b")
            drain(0.7)
            assert "LIVE_RESULT_COMPLETE" in frame("returned-"+title), "Esc did not return to root"
        anchor = footer_row()
        assert anchor >= height-2, "footer is not anchored at terminal bottom"
        history_before = list(screen.display)
        os.write(master,b"/")
        drain()
        menu = frame("command-overlay")
        assert "/agents" in menu or "/add-dir" in menu, "command menu missing"
        assert footer_row() == anchor, "command menu moved footer"
        menu_top = next(y for y,line in enumerate(screen.display) if line.strip() and set(line.strip()) == {"─"})
        assert list(screen.display)[:menu_top] == history_before[:menu_top], "command menu moved uncovered history"
        os.write(master,b"\x1b")
        drain()
        os.write(master,b"\x7f")
        drain()
        assert list(screen.display) == history_before, "closing command menu moved existing rows"
        os.write(master, b"/effort\r")
        drain()
        effort = frame("effort")
        assert "Faster" in effort and "▲" in effort and "Enter" in effort
        os.write(master, b"\x1b[C")
        drain()
        frame("effort-adjusted")
        os.write(master, b"\x1b")
        drain()
        assert footer_row() == anchor, "closing effort moved composer"
        before = frame("after-effort")
        assert_composer_background()
        assert "PTY_RESULT_COMPLETE" in raw.decode("utf8", "replace"), "history never flushed"
        assert b"]ccdp-" not in raw, "private barrier leaked to terminal"
        os.write(master, b"[A[B")
        drain()
        after = frame("arrows")
        assert before == after, "empty-history arrows moved the screen"
        os.write(master, "draft 中文".encode())
        drain()
        frame("draft")
        assert_composer_background()
        os.write(master, b"\x1b\rsecond line")
        drain()
        frame("multiline")
        assert_composer_background(lines=2)
        os.write(master, b"")  # reader
        drain()
        frame("reader")
        assert b"[?1049h" in raw, "reader did not enter alternate screen"
        os.write(master, b"")
        drain()
        restored = frame("restored")
        assert "draft 中文" in restored, "reader lost draft"
        assert b"[?1049l" in raw, "reader did not restore ordinary screen"
        new_width = 40 if width > 40 else 80
        fcntl.ioctl(master, termios.TIOCSWINSZ, struct.pack("HHHH", height, new_width, 0, 0))
        screen.resize(lines=height, columns=new_width)
        os.kill(process.pid, signal.SIGWINCH)
        drain()
        resized = frame("resized")
        assert_composer_background(lines=2)
        assert "draft 中文" in resized, "resize lost composer"
        os.write(master, b"")
        drain(0.2)
        os.write(master, b"")
        drain(0.5)
        if "Exit ccdp?" in "\n".join(screen.display):
            os.write(master,b"y")
            drain(0.5)
        process.wait(timeout=4)
        assert process.returncode == 0, raw.decode("utf8", "replace")[-2000:]
        assert b"[?2004l" in raw, "paste mode not restored on exit"
        return {"size": [width, height], "frames": frames, "passed": True}
    finally:
        with open(f"/private/tmp/ccdp-pty-debug-{width}.json","w") as debug:
            json.dump({"frames":frames,"raw":raw.decode("utf8","replace")},debug,ensure_ascii=False,indent=2)
        if process.poll() is None:
            process.kill()
            process.wait()
        os.close(master)

results = [run_fixture(sys.argv[1], w, h) for w, h in [(80,24),(40,16),(120,40),(200,56)]]
with open(sys.argv[2], "w") as output:
    json.dump(results, output, ensure_ascii=False, indent=2)
print("PTY passed: 80x24, 40x16, 120x40, 200x56; live updates, arrows, history write, reader, draft, resize, exit, composer background (empty/typed/multiline)")
