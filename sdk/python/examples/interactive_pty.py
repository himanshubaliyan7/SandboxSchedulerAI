#!/usr/bin/env python3
"""An interactive shell in a sandbox, attached to your terminal.

    export SBX_API_KEY=sbx_...
    python examples/interactive_pty.py            # type `exit` or press Ctrl+D to leave

Your terminal is put into raw mode, so Ctrl+C, arrow keys and full-screen
programs go to the sandbox. Resizing the window resizes the remote terminal.
With stdin from a pipe the input is simply forwarded, which is handy for
scripts: `echo 'ls -la; exit' | python examples/interactive_pty.py`.
"""

from __future__ import annotations

import os
import select
import shutil
import signal
import sys
import threading

from sbx import PtyHandle, Sandbox, SbxError

TEMPLATE = os.environ.get("SBX_TEMPLATE", "python")


def pump(shell: PtyHandle, done: threading.Event, interactive: bool) -> None:
    """Forward local input to the remote terminal until the shell exits."""
    fd = sys.stdin.fileno()
    input_open = True
    while not done.is_set():
        if not input_open:
            done.wait(0.1)
            continue
        readable, _, _ = select.select([fd], [], [], 0.1)
        if not readable:
            continue
        data = os.read(fd, 4096)
        try:
            if data:
                shell.send_input(data)
            else:  # end of piped input: send Ctrl+D and let the shell finish
                input_open = False
                if not interactive:
                    shell.send_input(b"\x04")
        except SbxError:
            return


def watch(shell: PtyHandle, done: threading.Event) -> None:
    try:
        shell.wait()
    except SbxError:
        pass
    finally:
        done.set()


def main() -> int:
    size = shutil.get_terminal_size((80, 24))
    interactive = sys.stdin.isatty()

    def show(data: bytes) -> None:  # runs on the SDK's reader thread
        sys.stdout.buffer.write(data)
        sys.stdout.buffer.flush()

    def resized(*_: object) -> None:
        now = shutil.get_terminal_size()
        try:
            shell.resize(now.columns, now.lines)
        except SbxError:
            pass

    with Sandbox.create(TEMPLATE, timeout=600) as sandbox:
        term = os.environ.get("TERM", "xterm")
        shell = sandbox.pty.create(size.columns, size.lines, show, envs={"TERM": term})
        done = threading.Event()
        threading.Thread(target=watch, args=(shell, done), daemon=True).start()

        if interactive:
            import termios
            import tty

            fd = sys.stdin.fileno()
            saved = termios.tcgetattr(fd)
            signal.signal(signal.SIGWINCH, resized)
            tty.setraw(fd)
            try:
                pump(shell, done, interactive)
            finally:
                termios.tcsetattr(fd, termios.TCSADRAIN, saved)
        else:
            pump(shell, done, interactive)
        result = shell.wait(timeout=10)
        print(f"\r\n[shell exited with code {result.exit_code}]", file=sys.stderr)
        return result.exit_code


if __name__ == "__main__":
    sys.exit(main())
