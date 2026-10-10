"""The sandbox object: lifecycle and the entry point to commands, files and PTYs."""

from __future__ import annotations

from builtins import list as List
from collections.abc import Mapping, Sequence
from typing import Any

import httpx

from .._config import ConnectionConfig, RetryPolicy, ms, seg
from ..errors import ConfigurationError, NotFoundError, SandboxNotRunningError, SbxError
from ..models import NetworkPolicy, SandboxEvent, SandboxInfo, SandboxStats, TemplateInfo
from .api import AsyncApiClient
from .commands import AsyncCommands
from .files import AsyncFiles
from .pty import AsyncPty


def _api(
    *,
    api_key: str | None,
    base_url: str | None,
    access_token: str | None = None,
    request_timeout: float | None,
    retry: RetryPolicy | None,
    headers: Mapping[str, str] | None,
    http_client: httpx.AsyncClient | None,
    transport: httpx.AsyncBaseTransport | None,
    ws_base_url: str | None = None,
) -> AsyncApiClient:
    config = ConnectionConfig.resolve(
        api_key=api_key,
        base_url=base_url,
        access_token=access_token,
        request_timeout=request_timeout,
        retry=retry,
        headers=headers,
        ws_base_url=ws_base_url,
    )
    return AsyncApiClient(config, http_client=http_client, transport=transport)


class AsyncSandbox:
    """A running sandbox.

    Create one with :meth:`create`, or attach to an existing one with
    :meth:`connect`. Used as a context manager, the sandbox is killed on
    exit. ``commands``, ``files`` and ``pty`` do the work inside it.

    Credentials: the tenant API key (``api_key=`` or ``SBX_API_KEY``) is
    needed for lifecycle calls; commands, files and PTYs use the sandbox's
    own access token whenever the client has it. Neither is ever logged or
    included in ``repr``.
    """

    def __init__(
        self,
        sandbox_id: str,
        api: AsyncApiClient,
        info: SandboxInfo | None = None,
    ) -> None:
        self._sandbox_id = sandbox_id
        self._api = api
        self._info = info
        self.commands = AsyncCommands(api, sandbox_id)
        self.files = AsyncFiles(api, sandbox_id)
        self.pty = AsyncPty(api, sandbox_id)

    def __repr__(self) -> str:
        return f"<{type(self).__name__} {self._sandbox_id!r} at {self._api.config.base_url}>"

    # -- identity -------------------------------------------------------------

    @property
    def sandbox_id(self) -> str:
        return self._sandbox_id

    @property
    def access_token(self) -> str | None:
        """The sandbox's access token: enough, with the id, for :meth:`connect`. Treat it as a secret."""
        return self._api.config.access_token

    @property
    def info(self) -> SandboxInfo | None:
        """The sandbox as last described by the server (``None`` after a token-only connect)."""
        return self._info

    @property
    def template_id(self) -> str | None:
        return self._info.template_id if self._info is not None else None

    @property
    def config(self) -> ConnectionConfig:
        return self._api.config

    def _path(self, suffix: str = "") -> str:
        return f"/v1/sandboxes/{seg(self._sandbox_id)}{suffix}"

    # -- construction ---------------------------------------------------------

    @classmethod
    async def create(
        cls,
        template: str,
        *,
        timeout: float | None = None,
        envs: Mapping[str, str] | None = None,
        metadata: Mapping[str, str] | None = None,
        network: NetworkPolicy | Mapping[str, Any] | str | None = None,
        api_key: str | None = None,
        base_url: str | None = None,
        request_timeout: float | None = None,
        retry: RetryPolicy | None = None,
        headers: Mapping[str, str] | None = None,
        http_client: httpx.AsyncClient | None = None,
        transport: httpx.AsyncBaseTransport | None = None,
        ws_base_url: str | None = None,
    ) -> AsyncSandbox:
        """Claim a warm sandbox of ``template``.

        Args:
            template: template id, as configured with ``sbxd admin template``.
            timeout: maximum lifetime in seconds; ``None`` for the
                template's. It cannot exceed the template's limit.
            envs: environment of every command in the sandbox.
            metadata: free-form labels, usable as a filter in :meth:`list`.
            network: egress policy: a :class:`~sbx.NetworkPolicy`, a mapping
                with the same keys, or just a mode (``"none"``,
                ``"allowlist"``, ``"full"``). Never looser than the template's.
            api_key: tenant API key; defaults to ``SBX_API_KEY``.
            base_url: server URL; defaults to ``SBX_BASE_URL``, then
                ``http://127.0.0.1:8080``.
            request_timeout: seconds allowed for one ordinary HTTP request.
            retry: the :class:`~sbx.RetryPolicy`; ``RetryPolicy.none()``
                switches automatic retries off.
            headers: extra headers for every request.
            http_client, transport: bring your own ``httpx`` client or
                transport (proxies, TLS settings, tests). A client passed
                in is not closed by the SDK.
            ws_base_url: WebSocket origin if it differs from ``base_url``.

        ``429 rate_limited`` and ``503 pool_empty`` are retried according to
        ``retry``, honouring ``Retry-After``.
        """
        body: dict[str, Any] = {"template_id": template}
        timeout_ms = ms(timeout)
        if timeout_ms:
            body["timeout_ms"] = timeout_ms
        if envs:
            body["env"] = dict(envs)
        if metadata:
            body["metadata"] = dict(metadata)
        if network is not None:
            body["network"] = NetworkPolicy.coerce(network).to_json()
        api = _api(
            api_key=api_key,
            base_url=base_url,
            request_timeout=request_timeout,
            retry=retry,
            headers=headers,
            http_client=http_client,
            transport=transport,
            ws_base_url=ws_base_url,
        )
        try:
            data = await api.json("POST", "/v1/sandboxes", plane="control", json=body, is_create=True)
        except BaseException:
            await api.close()
            raise
        token = data.get("access_token")
        info = SandboxInfo.from_json(data)
        if not info.sandbox_id:
            await api.close()
            raise SbxError("the server answered a sandbox without an id")
        if isinstance(token, str) and token:
            from dataclasses import replace

            api.config = replace(api.config, access_token=token)
        return cls(info.sandbox_id, api, info)

    @classmethod
    async def connect(
        cls,
        sandbox_id: str,
        *,
        access_token: str | None = None,
        api_key: str | None = None,
        base_url: str | None = None,
        request_timeout: float | None = None,
        retry: RetryPolicy | None = None,
        headers: Mapping[str, str] | None = None,
        http_client: httpx.AsyncClient | None = None,
        transport: httpx.AsyncBaseTransport | None = None,
        ws_base_url: str | None = None,
    ) -> AsyncSandbox:
        """Attach to an existing sandbox.

        The sandbox id and its ``access_token`` are sufficient for commands,
        files and PTYs. Lifecycle calls (``kill``, ``set_timeout``,
        ``get_info``, ``update_network``, ``events``, ``stats``) need the
        tenant API key as well.

        The sandbox is checked once: with an API key its description is
        fetched; with only a token, its command list. A sandbox that does
        not exist, is not running, or does not accept the credentials
        raises here rather than on first use.
        """
        api = _api(
            api_key=api_key,
            base_url=base_url,
            access_token=access_token,
            request_timeout=request_timeout,
            retry=retry,
            headers=headers,
            http_client=http_client,
            transport=transport,
            ws_base_url=ws_base_url,
        )
        if not api.config.api_key and not api.config.access_token:
            await api.close()
            raise ConfigurationError("connect needs access_token= or an API key (api_key= or SBX_API_KEY)")
        sandbox = cls(sandbox_id, api)
        try:
            if api.config.api_key:
                await sandbox.get_info()
            else:
                await sandbox.commands.list()
        except BaseException:
            await api.close()
            raise
        return sandbox

    @classmethod
    async def list(
        cls,
        *,
        state: str | Sequence[str] | None = None,
        template: str | None = None,
        metadata: Mapping[str, str] | None = None,
        limit: int | None = None,
        api_key: str | None = None,
        base_url: str | None = None,
        request_timeout: float | None = None,
        retry: RetryPolicy | None = None,
        headers: Mapping[str, str] | None = None,
        http_client: httpx.AsyncClient | None = None,
        transport: httpx.AsyncBaseTransport | None = None,
    ) -> List[SandboxInfo]:
        """List the tenant's sandboxes, newest first.

        Args:
            state: one state, several, or ``"all"``. By default only live
                sandboxes are listed. Terminated ones are kept for 7 days.
            template: only sandboxes of this template.
            metadata: only sandboxes whose metadata contains every pair.
            limit: at most this many (server default 100, maximum 1000).
        """
        params: dict[str, str | int | List[str]] = {}
        if state is not None:
            params["state"] = state if isinstance(state, str) else ",".join(state)
        if template:
            params["template_id"] = template
        if metadata:
            params["metadata"] = [f"{k}={v}" for k, v in metadata.items()]
        if limit is not None:
            params["limit"] = int(limit)
        api = _api(
            api_key=api_key,
            base_url=base_url,
            request_timeout=request_timeout,
            retry=retry,
            headers=headers,
            http_client=http_client,
            transport=transport,
        )
        try:
            data = await api.json("GET", "/v1/sandboxes", plane="control", params=params, idempotent=True)
        finally:
            await api.close()
        items = data.get("sandboxes")
        return (
            [SandboxInfo.from_json(s) for s in items if isinstance(s, Mapping)]
            if isinstance(items, List)
            else []
        )

    @classmethod
    async def list_templates(
        cls,
        *,
        api_key: str | None = None,
        base_url: str | None = None,
        request_timeout: float | None = None,
        retry: RetryPolicy | None = None,
        headers: Mapping[str, str] | None = None,
        http_client: httpx.AsyncClient | None = None,
        transport: httpx.AsyncBaseTransport | None = None,
    ) -> List[TemplateInfo]:
        """The templates sandboxes can be created from."""
        api = _api(
            api_key=api_key,
            base_url=base_url,
            request_timeout=request_timeout,
            retry=retry,
            headers=headers,
            http_client=http_client,
            transport=transport,
        )
        try:
            data = await api.json("GET", "/v1/templates", plane="control", idempotent=True)
        finally:
            await api.close()
        items = data.get("templates")
        return (
            [TemplateInfo.from_json(t) for t in items if isinstance(t, Mapping)]
            if isinstance(items, List)
            else []
        )

    # -- lifecycle ------------------------------------------------------------

    async def get_info(self) -> SandboxInfo:
        """Fetch the sandbox's current description. Works after it has terminated, too."""
        self._info = SandboxInfo.from_json(
            await self._api.json("GET", self._path(), plane="control", idempotent=True)
        )
        return self._info

    async def is_running(self) -> bool:
        """Whether the sandbox is running right now."""
        if self._api.config.api_key:
            try:
                return (await self.get_info()).state == "running"
            except NotFoundError:
                return False
        try:
            await self.commands.list()
        except SandboxNotRunningError:
            return False
        return True

    async def set_timeout(self, timeout: float) -> SandboxInfo:
        """Make the sandbox end ``timeout`` seconds from now (shorter or longer than before).

        The server caps the result at the template's maximum lifetime; read
        ``expires_at`` of the returned info for the effective value.
        """
        timeout_ms = ms(timeout)
        if not timeout_ms:
            raise ValueError("timeout must be positive")
        self._info = SandboxInfo.from_json(
            await self._api.json(
                "POST",
                self._path("/timeout"),
                plane="control",
                json={"timeout_ms": timeout_ms},
                idempotent=True,
            )
        )
        return self._info

    async def update_network(self, network: NetworkPolicy | Mapping[str, Any] | str) -> SandboxInfo:
        """Replace the egress policy of the running sandbox.

        Takes effect before the call returns. The mode may be the template's
        or stricter; a sandbox created with mode ``none`` has no egress
        proxy and cannot be given one (:class:`~sbx.ConflictError`,
        ``network_not_attached``).
        """
        body = NetworkPolicy.coerce(network).to_json()
        self._info = SandboxInfo.from_json(
            await self._api.json("PUT", self._path("/network"), plane="control", json=body, idempotent=True)
        )
        return self._info

    async def events(self, type: str | None = None) -> List[SandboxEvent]:
        """The sandbox's event log, oldest first: state changes and one
        ``command_started`` / ``command_finished`` record per command."""
        params = {"type": type} if type else None
        data = await self._api.json(
            "GET", self._path("/events"), plane="control", params=params, idempotent=True
        )
        items = data.get("events")
        return (
            [SandboxEvent.from_json(e) for e in items if isinstance(e, Mapping)]
            if isinstance(items, List)
            else []
        )

    async def stats(self) -> SandboxStats:
        """Memory, CPU and pids counters of the running sandbox."""
        return SandboxStats.from_json(
            await self._api.json("GET", self._path("/stats"), plane="control", idempotent=True)
        )

    async def kill(self, *, wait: bool = False) -> bool:
        """Kill the sandbox and destroy its storage.

        Returns ``True`` if the server accepted the request and ``False``
        if there is no such sandbox. By default the call returns as soon as
        the sandbox is fenced (unusable, no longer counted against the
        quota); the container is removed in the background. ``wait=True``
        asks the server to answer only after the container is gone.
        """
        params = {"wait": "true"} if wait else None
        try:
            await self._api.request("DELETE", self._path(), plane="control", params=params, idempotent=True)
        except NotFoundError:
            return False
        return True

    async def close(self) -> None:
        """Release the HTTP connections. The sandbox keeps running."""
        await self._api.close()

    async def __aenter__(self) -> AsyncSandbox:
        return self

    async def __aexit__(self, exc_type: object, exc: object, tb: object) -> None:
        try:
            if self._api.config.api_key:
                await self.kill()
        except SbxError:
            # Do not hide the exception that is already propagating.
            if exc_type is None:
                raise
        finally:
            await self.close()
