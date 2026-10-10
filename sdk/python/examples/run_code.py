#!/usr/bin/env python3
"""Run LLM-style Python code in a sandbox and print what it produced.

export SBX_API_KEY=sbx_...            # tenant API key
export SBX_BASE_URL=http://127.0.0.1:8080
python examples/run_code.py
"""

from __future__ import annotations

import os

from sbx import CommandExitError, CommandTimeoutError, Sandbox

TEMPLATE = os.environ.get("SBX_TEMPLATE", "python")

# What a model might hand back when asked for the 20th Fibonacci number.
GENERATED_CODE = """
def fib(n):
    a, b = 0, 1
    for _ in range(n):
        a, b = b, a + b
    return a

print("fib(20) =", fib(20))
"""

BROKEN_CODE = "import json\nprint(json.loads('{not json}'))\n"


def run_python(sandbox: Sandbox, code: str) -> str:
    """Execute code and return a text observation for the model."""
    sandbox.files.write("main.py", code)
    try:
        # No shell: `python3 main.py` with a 30 second limit enforced by the sandbox.
        result = sandbox.commands.run("python3", args=["main.py"], timeout=30)
    except CommandTimeoutError:
        return "error: the code ran for more than 30 seconds and was killed"
    except CommandExitError as e:
        return f"error (exit code {e.exit_code}):\n{e.stderr}"
    return result.stdout


def main() -> None:
    # The sandbox is killed when the block ends, whatever happens inside it.
    with Sandbox.create(TEMPLATE, timeout=120, metadata={"example": "run_code"}) as sandbox:
        print(f"sandbox {sandbox.sandbox_id}")
        print("--- generated code")
        print(run_python(sandbox, GENERATED_CODE), end="")
        print("--- broken code")
        print(run_python(sandbox, BROKEN_CODE).strip().splitlines()[-1])
        print("--- streaming")
        sandbox.commands.run(
            "for i in 1 2 3; do echo step $i; sleep 0.2; done",
            on_stdout=lambda text: print("stdout:", text, end=""),
        )


if __name__ == "__main__":
    main()
