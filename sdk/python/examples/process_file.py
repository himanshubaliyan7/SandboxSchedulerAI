#!/usr/bin/env python3
"""Upload a file, process it in the sandbox, download the result.

export SBX_API_KEY=sbx_...
python examples/process_file.py
"""

from __future__ import annotations

import csv
import json
import os
import random
import tempfile

from sbx import Sandbox

TEMPLATE = os.environ.get("SBX_TEMPLATE", "python")

# The analysis an agent wants to run on the user's data.
SCRIPT = """
import csv, json, statistics
rows = list(csv.DictReader(open("input/sales.csv")))
by_region = {}
for row in rows:
    by_region.setdefault(row["region"], []).append(float(row["amount"]))
summary = {r: {"orders": len(v), "total": round(sum(v), 2), "median": round(statistics.median(v), 2)}
           for r, v in sorted(by_region.items())}
json.dump(summary, open("output/summary.json", "w"), indent=2)
print(f"processed {len(rows)} rows")
"""


def make_csv(path: str, rows: int = 50_000) -> None:
    rng = random.Random(1)
    with open(path, "w", newline="") as f:
        writer = csv.writer(f)
        writer.writerow(["order_id", "region", "amount"])
        for i in range(rows):
            writer.writerow([i, rng.choice(["north", "south", "east", "west"]), f"{rng.uniform(5, 500):.2f}"])


def main() -> None:
    with tempfile.TemporaryDirectory() as tmp, Sandbox.create(TEMPLATE, timeout=120) as sandbox:
        local = os.path.join(tmp, "sales.csv")
        make_csv(local)

        # Upload: an open file is streamed, not read into memory.
        with open(local, "rb") as f:
            uploaded = sandbox.files.write("input/sales.csv", f)
        print(f"uploaded {uploaded.path} ({uploaded.size} bytes)")
        sandbox.files.write_files([("analyse.py", SCRIPT), ("output/.keep", b"")])

        result = sandbox.commands.run("python3 analyse.py")
        print(result.stdout, end="")

        for entry in sandbox.files.list("output"):
            print(f"  output/{entry.name}  {entry.size} bytes")

        # Download: as text, or chunk by chunk for large files.
        summary = json.loads(sandbox.files.read("output/summary.json"))
        for region, numbers in summary.items():
            print(f"  {region:<6} {numbers}")
        saved = os.path.join(tmp, "summary.json")
        with open(saved, "wb") as out:
            for chunk in sandbox.files.read("output/summary.json", format="stream"):
                out.write(chunk)
        with open(saved) as f:
            assert json.load(f) == summary
        print(f"downloaded {os.path.getsize(saved)} bytes")


if __name__ == "__main__":
    main()
