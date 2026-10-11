"""Files: read, write, list and remove inside the sandbox."""

from __future__ import annotations

import posixpath
from builtins import list as List
from collections.abc import AsyncIterator, Iterable, Mapping
from typing import Any, Literal, Union, overload

import httpx

from .._config import describe, seg
from ..errors import ApiError, ConflictError, PathNotFoundError, TransportError
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

        Returns a :class:`~sbx.WriteInfo`: the file's name, its absolute
        path and the number of bytes written.
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
            return WriteInfo.for_path(path, sent[0])

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
        return WriteInfo.for_path(path, sent[0])

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

    async def get_info(self, path: str) -> EntryInfo:
        """Describe the file or directory at ``path`` (a symbolic link is followed).

        Raises :class:`~sbx.PathNotFoundError` if nothing is there. Uses the
        server's stat route; against a server, or a sandbox agent, that
        predates it, falls back to listing the parent directory.
        """
        try:
            data = await self._api.json(
                "GET", self._base + "/stat", plane="data", params={"path": path}, idempotent=True
            )
        except ApiError as e:
            if not _stat_unsupported(e):
                raise
        else:
            return EntryInfo.from_json(data)
        clean = posixpath.normpath(path)
        parent, name = posixpath.split(clean)
        if not name or name in (".", ".."):
            # The path names a directory by itself: listing it proves it exists.
            await self.list(clean)
            return EntryInfo(name=name or "/", path=clean, is_dir=True)
        for entry in await self.list(parent or "."):
            if entry.name == name:
                return entry
        raise PathNotFoundError(f"{path}: no such file or directory", code="file_not_found", status_code=404)

    async def stat(self, path: str) -> EntryInfo:
        """Alias of :meth:`get_info`."""
        return await self.get_info(path)

    async def exists(self, path: str) -> bool:
        """Whether a file or directory exists at ``path``."""
        try:
            await self.get_info(path)
        except PathNotFoundError:
            return False
        except ConflictError as e:
            if e.code == "conflict":  # a path component is not a directory
                return False
            raise
        return True

    async def make_dir(self, path: str) -> bool:
        """Create a directory and any missing parents.

        Returns ``True`` if this call created the directory and ``False`` if
        it already existed (as in E2B). A server that predates the
        distinction always answers "already existed", so there the result
        is always ``False``.
        """
        response = await self._api.request(
            "POST", self._base + "/mkdir", plane="data", params={"path": path}, idempotent=True
        )
        return response.status_code == 201

    async def remove(self, path: str) -> None:
        """Delete a file, or a directory and everything in it."""
        await self._api.request("DELETE", self._base, plane="data", params={"path": path})


def _stat_unsupported(e: ApiError) -> bool:
    """Whether an answer means "this server, or this sandbox's agent, has no stat route"."""
    if isinstance(e, PathNotFoundError):
        return False  # the real answer of a server that has the route
    if e.status_code == 404:
        return e.code != "file_not_found" and str(e).startswith("no such route")
    if e.status_code == 405:
        return True
    return e.code in ("not_implemented", "no_route")  # a new server in front of an old agent


async def _read_chunks(reader: Any, sent: List[int]) -> AsyncIterator[bytes]:
    while True:
        chunk = reader.read(CHUNK)
        if not chunk:
            return
        if isinstance(chunk, str):
            chunk = chunk.encode("utf-8")
        sent[0] += len(chunk)
        yield bytes(chunk)
