# Generated from tests/_async/test_files.py by scripts/unasync.py. Do not edit.
"""sandbox.files."""

from __future__ import annotations

import io
import os

import httpx
import pytest

import sbx
from sbx import Sandbox, WriteEntry, WriteInfo

from ..fake import ChunkStream, FakeServer, error


class OneShot:
    """A file-like object that cannot be rewound."""

    def __init__(self, data: bytes) -> None:
        self._buffer = io.BytesIO(data)
        self.reads = 0

    def read(self, n: int = -1) -> bytes:
        self.reads += 1
        return self._buffer.read(n)

    def seekable(self) -> bool:
        return False


def test_write_and_read_text_and_bytes(sandbox: Sandbox, server: FakeServer) -> None:
    info = sandbox.files.write("notes/hello.txt", "héllo\n")
    assert info == WriteInfo(path="notes/hello.txt", size=7)
    request = server.seen("PUT", r"/files$")[0]
    assert request.url.params["path"] == "notes/hello.txt" and "mode" not in request.url.params
    assert request.headers["content-type"] == "application/octet-stream"
    assert server.sandbox().files["/workspace/notes/hello.txt"] == "héllo\n".encode()

    assert sandbox.files.read("notes/hello.txt") == "héllo\n"
    assert sandbox.files.read("/workspace/notes/hello.txt", format="bytes") == "héllo\n".encode()
    assert sandbox.files.read("notes/hello.txt", "text") == "héllo\n"

    blob = bytes(range(256)) * 16
    assert (sandbox.files.write("/tmp/blob.bin", blob, mode=0o600)).size == len(blob)
    assert server.requests[-1].url.params["mode"] == "0600"
    assert sandbox.files.read("/tmp/blob.bin", format="bytes") == blob
    sandbox.files.write("/tmp/b2", bytearray(b"ab"), mode="0755")
    assert server.requests[-1].url.params["mode"] == "0755"
    sandbox.files.write("/tmp/b3", memoryview(b"abc"))
    assert server.sandbox().files["/tmp/b3"] == b"abc"

    with pytest.raises(ValueError):
        sandbox.files.read("notes/hello.txt", format="json")  # type: ignore[call-overload]
    with pytest.raises(ValueError):
        sandbox.files.write("x", b"", mode=0o7777)
    with pytest.raises(TypeError):
        sandbox.files.write("x", 42)  # type: ignore[arg-type]


def test_write_streams_file_like_objects(
    sandbox: Sandbox, server: FakeServer, tmp_path: os.PathLike[str]
) -> None:
    payload = os.urandom(300_000)  # several 64 KiB chunks
    info = sandbox.files.write("up/a.bin", io.BytesIO(payload))
    assert info.size == len(payload)
    request = server.requests[-1]
    # Streamed: no Content-Length, the body goes out in chunks.
    assert request.headers.get("transfer-encoding") == "chunked" and "content-length" not in request.headers
    assert server.sandbox().files["/workspace/up/a.bin"] == payload

    path = os.path.join(tmp_path, "local.bin")
    with open(path, "wb") as f:
        f.write(payload)
    with open(path, "rb") as f:
        f.seek(100)  # upload starts where the file object stands
        assert (sandbox.files.write("up/b.bin", f)).size == len(payload) - 100
    assert server.sandbox().files["/workspace/up/b.bin"] == payload[100:]

    assert (sandbox.files.write("up/c.txt", io.StringIO("téxt " * 20_000))).size == 6 * 20_000
    assert server.sandbox().files["/workspace/up/c.txt"] == ("téxt " * 20_000).encode()

    one = OneShot(payload)
    assert (sandbox.files.write("up/d.bin", one)).size == len(payload)
    assert one.reads > 4 and server.sandbox().files["/workspace/up/d.bin"] == payload


def test_upload_retries_only_what_can_be_rewound(
    sandbox: Sandbox, server: FakeServer, sleeps: list[float]
) -> None:
    payload = os.urandom(200_000)
    server.inject("PUT", r"/files$", error(429, "rate_limited", retry_after=1))
    sandbox.files.write("a.bin", io.BytesIO(payload))
    assert server.sandbox().files["/workspace/a.bin"] == payload and sleeps == [1.0]

    server.inject("PUT", r"/files$", error(502, "agent_unavailable", retry_after=1))
    sandbox.files.write("b.bin", payload)
    assert server.sandbox().files["/workspace/b.bin"] == payload and sleeps == [1.0, 1.0]

    server.inject("PUT", r"/files$", error(429, "rate_limited", retry_after=1))
    with pytest.raises(sbx.RateLimitError):
        sandbox.files.write("c.bin", OneShot(payload))
    assert "/workspace/c.bin" not in server.sandbox().files and sleeps == [1.0, 1.0]


def test_read_stream(sandbox: Sandbox, server: FakeServer) -> None:
    payload = os.urandom(200_000)
    sandbox.files.write("big.bin", payload)
    stream = sandbox.files.read("big.bin", format="stream")
    assert stream.size == len(payload)
    chunks = [chunk for chunk in stream]
    assert b"".join(chunks) == payload and len(chunks) > 1

    with sandbox.files.read("big.bin", format="stream") as partial:
        for chunk in partial:
            assert chunk == payload[: len(chunk)]
            break

    with pytest.raises(sbx.PathNotFoundError):  # raised by the call, not by the first iteration
        sandbox.files.read("missing.bin", format="stream")


def test_a_download_cut_short_is_an_error(sandbox: Sandbox, server: FakeServer, sleeps: list[float]) -> None:
    def broken() -> httpx.Response:
        return httpx.Response(200, headers={"Content-Length": "10"}, stream=ChunkStream([b"abc"], fail=True))

    server.inject("GET", r"/files$", broken())
    stream = sandbox.files.read("x", format="stream")
    with pytest.raises(sbx.TransportError, match="cut short"):
        _ = [chunk for chunk in stream]

    # A whole-file read is idempotent, so it is simply retried.
    sandbox.files.write("x", b"0123456789")
    server.inject("GET", r"/files$", broken())
    assert sandbox.files.read("x", format="bytes") == b"0123456789"
    assert len(sleeps) == 1
    server.inject("GET", r"/files$", *[broken() for _ in range(4)])
    with pytest.raises(sbx.TransportError):
        sandbox.files.read("x", format="bytes")


def test_write_files(sandbox: Sandbox, server: FakeServer) -> None:
    written = sandbox.files.write_files(
        [
            WriteEntry(path="a.txt", data="A", mode=0o640),
            ("b/b.bin", b"BB"),
            {"path": "/tmp/c.txt", "data": io.BytesIO(b"CCC"), "mode": "0600"},
            {"path": "d.txt", "data": "D"},
        ]
    )
    assert written == [
        WriteInfo("a.txt", 1),
        WriteInfo("b/b.bin", 2),
        WriteInfo("/tmp/c.txt", 3),
        WriteInfo("d.txt", 1),
    ]
    fake = server.sandbox()
    assert fake.files == {
        "/workspace/a.txt": b"A",
        "/workspace/b/b.bin": b"BB",
        "/tmp/c.txt": b"CCC",
        "/workspace/d.txt": b"D",
    }
    assert (fake.modes["/workspace/a.txt"], fake.modes["/tmp/c.txt"], fake.modes["/workspace/d.txt"]) == (
        "0640",
        "0600",
        "0644",
    )
    assert sandbox.files.write_files([]) == []


def test_list_exists_make_dir_remove(sandbox: Sandbox, server: FakeServer) -> None:
    assert sandbox.files.list() == []
    assert server.requests[-1].url.params["path"] == "."
    sandbox.files.write("data/in.csv", "a,b\n")
    sandbox.files.make_dir("data/out/deep")
    sandbox.files.make_dir("data/out/deep")  # already there: fine
    entries = sandbox.files.list("data")
    assert [(e.name, e.path, e.is_dir, e.type, e.size, e.mode) for e in entries] == [
        ("in.csv", "/workspace/data/in.csv", False, "file", 4, "0644"),
        ("out", "/workspace/data/out", True, "dir", 0, "0755"),
    ]
    assert entries[0].modified_time is not None

    assert sandbox.files.exists("data/in.csv")
    assert sandbox.files.exists("data")
    assert sandbox.files.exists("data/out/deep/")
    assert sandbox.files.exists("/workspace")
    assert sandbox.files.exists(".")
    assert not sandbox.files.exists("data/nope.csv")
    assert not sandbox.files.exists("nope/in.csv")
    assert not sandbox.files.exists("data/in.csv/child")  # the parent is a file

    with pytest.raises(sbx.PathNotFoundError) as e:
        sandbox.files.list("nope")
    assert e.value.code == "file_not_found"

    sandbox.files.remove("data/in.csv")
    assert not sandbox.files.exists("data/in.csv")
    with pytest.raises(sbx.PathNotFoundError):
        sandbox.files.remove("data/in.csv")
    sandbox.files.remove("data")
    assert not sandbox.files.exists("data/out")


def test_file_errors(sandbox: Sandbox, server: FakeServer) -> None:
    with pytest.raises(sbx.PathNotFoundError):
        sandbox.files.read("missing.txt")
    sandbox.files.make_dir("d")
    with pytest.raises(sbx.ConflictError) as c:
        sandbox.files.read("d")
    assert c.value.code == "is_directory"
    with pytest.raises(sbx.ConflictError):
        sandbox.files.write("d", "x")
    with pytest.raises(sbx.PermissionDeniedError):
        sandbox.files.write("/etc/passwd", "x")
    with pytest.raises(sbx.NoSpaceError) as n:
        sandbox.files.write("huge.bin", b"\0" * (8 * 1024 * 1024 + 1))
    assert n.value.status_code == 507
    server.inject("PUT", r"/files$", error(413, "payload_too_large", "upload exceeds 268435456 bytes"))
    with pytest.raises(sbx.PayloadTooLargeError):
        sandbox.files.write("x", b"y")
    with pytest.raises(sbx.InvalidArgumentError):
        sandbox.files.read("")
