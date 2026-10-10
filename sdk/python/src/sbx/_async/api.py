"""HTTP plumbing: credentials, retries and error mapping."""

from __future__ import annotations

import json as jsonlib
from collections.abc import AsyncIterator, Callable, Mapping
from typing import Any, Union

import httpx

from .._config import ConnectionConfig, RetryPolicy, describe
from ..errors import ProtocolError, SandboxTimeoutError, SbxError, TransportError, error_from_envelope
from ._compat import sleep

#: A request body: bytes, or a factory that returns a fresh chunk iterator
#: for every attempt.
Body = Union[bytes, Callable[[], AsyncIterator[bytes]]]
Params = Mapping[str, Union[str, int, list[str]]]


def _retry_after(response: httpx.Response) -> float | None:
    value = response.headers.get("retry-after")
    if not value:
        return None
    try:
        seconds = float(value)
    except ValueError:
        return None
    return seconds if seconds >= 0 else None


def error_from_response(response: httpx.Response, body: bytes) -> SbxError:
    """Map an error response (whose body has been read) to an exception."""
    parsed: Any
    try:
        parsed = jsonlib.loads(body) if body else None
    except ValueError:
        text = body[:200].decode("utf-8", "replace").strip()
        parsed = {"message": f"HTTP {response.status_code}: {text}" if text else ""}
    return error_from_envelope(
        parsed,
        status_code=response.status_code,
        request_id=response.headers.get("x-request-id"),
        retry_after=_retry_after(response),
    )


class AsyncApiClient:
    """One server, one set of credentials, one ``httpx`` client."""

    def __init__(
        self,
        config: ConnectionConfig,
        *,
        http_client: httpx.AsyncClient | None = None,
        transport: httpx.AsyncBaseTransport | None = None,
    ) -> None:
        self.config = config
        self._owns_client = http_client is None
        if http_client is None:
            http_client = httpx.AsyncClient(transport=transport, follow_redirects=False, trust_env=True)
        self._http = http_client

    def __repr__(self) -> str:
        return f"<{type(self).__name__} base_url={self.config.base_url!r}>"

    async def close(self) -> None:
        if self._owns_client:
            await self._http.aclose()

    def _timeout(self, timeout: float | None, *, streaming: bool) -> httpx.Timeout:
        total = timeout if timeout is not None else self.config.request_timeout
        if streaming:
            # No overall deadline: only the gap between two chunks is bounded.
            return httpx.Timeout(total, read=self.config.stream_idle_timeout, write=total, pool=total)
        return httpx.Timeout(total)

    def _build(
        self,
        method: str,
        path: str,
        *,
        plane: str,
        params: Params | None,
        json: Any,
        content: Body | None,
        headers: Mapping[str, str] | None,
        timeout: float | None,
        streaming: bool,
    ) -> httpx.Request:
        hdrs = self.config.base_headers()
        hdrs.update(self.config.auth_headers(plane))
        if headers:
            hdrs.update(headers)
        body: Any = None
        if content is not None:
            body = content if isinstance(content, bytes) else content()
            hdrs.setdefault("Content-Type", "application/octet-stream")
        return self._http.build_request(
            method,
            self.config.url(path),
            params=dict(params) if params else None,
            json=json,
            content=body,
            headers=hdrs,
            timeout=self._timeout(timeout, streaming=streaming),
        )

    async def _send(
        self,
        method: str,
        path: str,
        *,
        plane: str,
        params: Params | None,
        json: Any,
        content: Body | None,
        headers: Mapping[str, str] | None,
        timeout: float | None,
        idempotent: bool,
        is_create: bool,
        streaming: bool,
        retries: bool = True,
    ) -> httpx.Response:
        policy = self.config.retry if retries else RetryPolicy.none()
        attempt = 0
        while True:
            request = self._build(
                method,
                path,
                plane=plane,
                params=params,
                json=json,
                content=content,
                headers=headers,
                timeout=timeout,
                streaming=streaming,
            )
            err: SbxError
            transport_failure = False
            try:
                response = await self._http.send(request, stream=True)
                try:
                    if response.status_code < 400:
                        if not streaming:
                            await response.aread()
                        return response
                    body = await response.aread()
                finally:
                    if not streaming or response.status_code >= 400:
                        await response.aclose()
                err = error_from_response(response, body)
                cause: BaseException | None = None
            except httpx.TimeoutException as e:
                err = SandboxTimeoutError(f"{method} {path} timed out ({describe(e)})")
                cause, transport_failure = e, True
            except httpx.TransportError as e:
                err = TransportError(f"{method} {path} failed: {describe(e)}")
                cause, transport_failure = e, True
            delay = policy.delay_for(
                err, attempt, idempotent=idempotent, is_create=is_create, transport_failure=transport_failure
            )
            if delay is None:
                raise err from cause
            attempt += 1
            await sleep(delay)

    async def request(
        self,
        method: str,
        path: str,
        *,
        plane: str,
        params: Params | None = None,
        json: Any = None,
        content: Body | None = None,
        headers: Mapping[str, str] | None = None,
        timeout: float | None = None,
        idempotent: bool = False,
        is_create: bool = False,
        retries: bool = True,
    ) -> httpx.Response:
        """Send a request and return the fully read response, or raise the mapped error.

        ``idempotent`` and ``is_create`` say which failures may be retried
        (see :class:`~sbx.RetryPolicy`); ``retries=False`` forbids all of
        them, for a body that cannot be sent twice.
        """
        return await self._send(
            method,
            path,
            plane=plane,
            params=params,
            json=json,
            content=content,
            headers=headers,
            timeout=timeout,
            idempotent=idempotent,
            is_create=is_create,
            streaming=False,
            retries=retries,
        )

    async def open_stream(
        self,
        method: str,
        path: str,
        *,
        plane: str,
        params: Params | None = None,
        json: Any = None,
        headers: Mapping[str, str] | None = None,
        timeout: float | None = None,
        idempotent: bool = False,
    ) -> httpx.Response:
        """Send a request and return the response with its body unread.

        The caller must close the response. Error responses are read and
        raised here, with the same retries as :meth:`request`.
        """
        return await self._send(
            method,
            path,
            plane=plane,
            params=params,
            json=json,
            content=None,
            headers=headers,
            timeout=timeout,
            idempotent=idempotent,
            is_create=False,
            streaming=True,
        )

    async def json(self, method: str, path: str, **kwargs: Any) -> Any:
        """Send a request and decode a JSON object answer."""
        response = await self.request(method, path, **kwargs)
        return decode_json(response)


def decode_json(response: httpx.Response) -> dict[str, Any]:
    try:
        value = jsonlib.loads(response.content)
    except ValueError as e:
        raise ProtocolError(f"the server answered invalid JSON (HTTP {response.status_code}): {e}") from None
    if not isinstance(value, dict):
        raise ProtocolError("the server answered JSON that is not an object")
    return value
