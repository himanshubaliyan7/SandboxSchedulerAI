"""Files: read, write, list and remove inside the sandbox."""

from __future__ import annotations

import posixpath
from builtins import list as List
from collections.abc import AsyncIterator, Iterable, Mapping
from typing import Any, Literal, Union, overload

import httpx

from .._config import describe, seg
from ..errors import ConflictError, PathNotFoundError, TransportError
from ..models import EntryInfo, FileData, WriteEntry, WriteInfo
from .api import AsyncApiClient

CHUNK = 64 * 1024

#: What ``write_files`` accepts for one file.
FileSpec = Union[WriteEntry, Mapping[str, Any], "tuple[str, FileData]"]


def _mode(mode: str | int | None) -> str | None:
    if mode is None:
        return None
    if isinstance(mode, int):
        if not 0 <= mode <= 0o777:
            raise ValueError("mode must be permission bits between 0 and 0o777")
        return format(mode, "04o")
    return mode


class AsyncFileStream:
    """A file being downloaded, as an iterator of byte chunks.

    Iterating to the end closes it; otherwise close it yourself (it is also
    a context manager). ``size`` is the file's size when the server
    announced it.
    """

    def __init__(self, response: httpx.Response, path: str) -> None:
        self._response = response
        self._path = path
        self._chunks: AsyncIterator[bytes] | None = None
        length = response.headers.get("content-length")
        self.size: int | None = int(length) if length is not None and length.isdigit() else None

    def __aiter__(self) -> AsyncFileStream:
        return self

    async def __anext__(self) -> bytes:
        if self._chunks is None:
            self._chunks = self._response.aiter_bytes(CHUNK)
        try:
            return await self._chunks.__anext__()
        except StopAsyncIteration:
            await self.aclose()
            raise
        except (httpx.TransportError, httpx.StreamError) as e:
            await self.aclose()
            # The server aborts the connection when a transfer fails midway,
            # so a short body is never mistaken for the whole file.
            raise TransportError(f"download of {self._path!r} was cut short: {describe(e)}") from e

    async def aclose(self) -> None:
        await self._response.aclose()

    async def __aenter__(self) -> AsyncFileStream:
        return self

    async def __aexit__(self, *exc: object) -> None:
        await self.aclose()


class AsyncFiles:
    """``sandbox.files``. Paths are absolute, or relative to ``/workspace``."""

    def __init__(self, api: AsyncApiClient, sandbox_id: str) -> None:
        self._api = api
        self._base = f"/v1/sandboxes/{seg(sandbox_id)}/files"

    @overload
    async def read(self, path: str, format: Literal["text"] = "text") -> str: ...
    @overload
    async def read(self, path: str, format: Literal["bytes"]) -> bytes: ...
    @overload
    async def read(self, path: str, format: Literal["stream"]) -> AsyncFileStream: ...

    async def read(
        self, path: str, format: Literal["text", "bytes", "stream"] = "text"
    ) -> str | bytes | AsyncFileStream:
        """Download a file.

        ``format="text"`` returns ``str`` (UTF-8), ``"bytes"`` returns
        ``bytes``, and ``"stream"`` returns an iterator of chunks that never
        holds the whole file in memory.
        """
        if format == "stream":
            response = await self._api.open_stream(
                "GET", self._base, plane="data", params={"path": path}, idempotent=True
            )
            return AsyncFileStream(response, path)
        if format not in ("text", "bytes"):
            raise ValueError("format must be 'text', 'bytes' or 'stream'")
        response = await self._api.request(
            "GET", self._base, plane="data", params={"path": path}, idempotent=True
        )
        return response.content if format == "bytes" else response.content.decode("utf-8")

    async def write(self, path: str, data: FileData, *, mode: str | int | None = None) -> WriteInfo:
        """Upload a file, creating parent directories and replacing an existing file.

        ``data`` may be ``str`` (written as UTF-8), bytes, or a file-like
        object, which is streamed in chunks and never read into memory as a
        whole. The file appears atomically when the upload is complete.
        """
        params: dict[str, str] = {"path": path}
        octal = _mode(mode)
        if octal is not None:
            params["mode"] = octal
        sent = [0]
        if isinstance(data, str):
            data = data.encode("utf-8")
        if isinstance(data, (bytes, bytearray, memoryview)):
            body = bytes(data)
            sent[0] = len(body)
            await self._api.request(
                "PUT", self._base, plane="data", params=params, content=body, idempotent=True
            )
            return WriteInfo(path=path, size=sent[0])

        reader: Any = data
        if not callable(getattr(reader, "read", None)):
            raise TypeError("data must be str, bytes or a file-like object with read()")
        start: int | None = None
        try:
            if reader.seekable():
                start = reader.tell()
        except (AttributeError, OSError, ValueError):
            start = None

        def chunks() -> AsyncIterator[bytes]:
            if start is not None:
                reader.seek(start)
            sent[0] = 0
            return _read_chunks(reader, sent)

        # A stream that cannot be rewound is sent exactly once: no retries.
        await self._api.request(
            "PUT",
            self._base,
            plane="data",
            params=params,
            content=chunks,
            idempotent=True,
            retries=start is not None,
        )
        return WriteInfo(path=path, size=sent[0])

    async def write_files(self, files: Iterable[FileSpec]) -> List[WriteInfo]:
        """Upload several files, one after another.

        Each item is a :class:`~sbx.WriteEntry`, a ``(path, data)`` tuple or
        a mapping with ``path``, ``data`` and optionally ``mode``.
        """
        out: List[WriteInfo] = []
        for item in files:
            if isinstance(item, WriteEntry):
                entry = item
            elif isinstance(item, Mapping):
                entry = WriteEntry(path=item["path"], data=item["data"], mode=item.get("mode"))
            else:
                entry = WriteEntry(path=item[0], data=item[1])  # type: ignore[arg-type]
            out.append(await self.write(entry.path, entry.data, mode=entry.mode))
        return out

    async def list(self, path: str = ".") -> List[EntryInfo]:
        """List a directory (not recursive), sorted by name."""
        data = await self._api.json(
            "GET", self._base + "/list", plane="data", params={"path": path}, idempotent=True
        )
        items = data.get("files")
        return (
            [EntryInfo.from_json(f) for f in items if isinstance(f, Mapping)]
            if isinstance(items, List)
            else []
        )

    async def exists(self, path: str) -> bool:
        """Whether a file or directory exists at ``path``."""
        clean = posixpath.normpath(path)
        parent, name = posixpath.split(clean)
        try:
            if not name or name in (".", ".."):
                await self.list(clean)
                return True
            entries = await self.list(parent or ".")
        except PathNotFoundError:
            return False
        except ConflictError as e:
            if e.code == "conflict":  # a path component is not a directory
                return False
            raise
        return any(entry.name == name for entry in entries)

    async def make_dir(self, path: str) -> None:
        """Create a directory and any missing parents. Succeeds if it already exists."""
        await self._api.request(
            "POST", self._base + "/mkdir", plane="data", params={"path": path}, idempotent=True
        )

    async def remove(self, path: str) -> None:
        """Delete a file, or a directory and everything in it."""
        await self._api.request("DELETE", self._base, plane="data", params={"path": path})


async def _read_chunks(reader: Any, sent: List[int]) -> AsyncIterator[bytes]:
    while True:
        chunk = reader.read(CHUNK)
        if not chunk:
            return
        if isinstance(chunk, str):
            chunk = chunk.encode("utf-8")
        sent[0] += len(chunk)
        yield bytes(chunk)
