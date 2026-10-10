#!/usr/bin/env python3
"""Generate the blocking flavour (``src/sbx/_sync``) from ``src/sbx/_async``,
and the blocking tests (``tests/_sync``) from ``tests/_async``.

The asyncio code is the single source of truth; this script rewrites it
token by token into the equivalent blocking code, so the two clients cannot
drift apart. ``_compat.py`` is the only module written by hand in both
flavours (asyncio primitives on one side, threads on the other).

    python scripts/unasync.py           # rewrite src/sbx/_sync and tests/_sync
    python scripts/unasync.py --check   # exit 1 if it is out of date
"""

from __future__ import annotations

import re
import subprocess
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
#: (source directory, generated directory): the library, and its tests.
TREES = [
    (ROOT / "src" / "sbx" / "_async", ROOT / "src" / "sbx" / "_sync"),
    (ROOT / "tests" / "_async", ROOT / "tests" / "_sync"),
]
HAND_WRITTEN = {"_compat.py"}

HEADER = "# Generated from {source} by scripts/unasync.py. Do not edit.\n"

# Applied in order. Longer, more specific patterns come first.
REPLACEMENTS: list[tuple[str, str]] = [
    (r"\basync def\b", "def"),
    (r"\basync with\b", "with"),
    (r"\basync for\b", "for"),
    (r"\bawait\s+", ""),
    (r"\bAsyncIterator\b", "Iterator"),
    (r"\bAsyncIterable\b", "Iterable"),
    (r"\bStopAsyncIteration\b", "StopIteration"),
    (r"\b__aenter__\b", "__enter__"),
    (r"\b__aexit__\b", "__exit__"),
    (r"\b__aiter__\b", "__iter__"),
    (r"\b__anext__\b", "__next__"),
    (r"\baclose\b", "close"),
    (r"\baread\b", "read"),
    (r"\baiter_bytes\b", "iter_bytes"),
    (r"\bhttpx\.AsyncClient\b", "httpx.Client"),
    (r"\bhttpx\.AsyncBaseTransport\b", "httpx.BaseTransport"),
    (r"\bAsync(Sandbox|Commands|CommandHandle|Files|FileStream|PtyHandle|Pty|ApiClient)\b", r"\1"),
    (r"\bsbx\._async\b", "sbx._sync"),
    (
        r"The asyncio flavour of the SDK\. ``sbx\._sync`` is generated from it\.",
        "The blocking flavour of the SDK.",
    ),
]


def convert(text: str, target: Path) -> str:
    for pattern, replacement in REPLACEMENTS:
        text = re.sub(pattern, replacement, text)
    # Renaming changes import order and removing ``await`` shortens lines:
    # re-sort and reformat so the output is clean under ``ruff``.
    for args in (["check", "--select", "I", "--fix-only", "--quiet"], ["format"]):
        done = subprocess.run(
            [sys.executable, "-m", "ruff", *args, "--stdin-filename", str(target), "-"],
            input=text,
            capture_output=True,
            text=True,
            cwd=ROOT,
            check=False,
        )
        if done.returncode != 0:
            raise SystemExit(f"ruff {args[0]} failed for {target.name}: {done.stderr.strip()}")
        text = done.stdout
    return text


def generate() -> tuple[dict[Path, str], list[Path]]:
    """The wanted content of every generated file, and generated files that should not exist."""
    wanted: dict[Path, str] = {}
    for src_dir, dst_dir in TREES:
        for src in sorted(src_dir.glob("*.py")):
            if src.name in HAND_WRITTEN:
                continue
            header = HEADER.format(source=src.relative_to(ROOT).as_posix())
            wanted[dst_dir / src.name] = convert(header + src.read_text(), dst_dir / src.name)
    extra = [
        path
        for _, dst_dir in TREES
        for path in dst_dir.glob("*.py")
        if path.name not in HAND_WRITTEN and path not in wanted
    ]
    return wanted, extra


def main(argv: list[str]) -> int:
    check = "--check" in argv
    wanted, extra = generate()
    stale = [path for path, text in wanted.items() if not path.exists() or path.read_text() != text]
    if check:
        if stale or extra:
            names = ", ".join(p.relative_to(ROOT).as_posix() for p in [*stale, *extra])
            print(f"generated code is out of date; run scripts/unasync.py: {names}", file=sys.stderr)
            return 1
        return 0
    for path in stale:
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(wanted[path])
    for path in extra:
        path.unlink()
    print(f"{len(wanted)} generated files, {len(stale)} rewritten, {len(extra)} removed")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
