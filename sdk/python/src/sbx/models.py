"""Plain data types returned by the SDK.

Everything parsed from the server keeps the original JSON in ``raw`` and
tolerates unknown fields and unknown enum values: states, reasons and event
types are plain strings, never closed enums.
"""

from __future__ import annotations

import re
from collections.abc import Mapping, Sequence
from dataclasses import dataclass, field
from datetime import datetime, timezone
from typing import IO, Any, Union

__all__ = [
    "CommandEvent",
    "CommandInfo",
    "CommandResult",
    "EntryInfo",
    "ExitEvent",
    "ExitStatus",
    "NetworkPolicy",
    "PtyOutputEvent",
    "SandboxEvent",
    "SandboxInfo",
    "SandboxStats",
    "StderrEvent",
    "StdoutEvent",
    "TemplateInfo",
    "TruncatedEvent",
    "WriteEntry",
    "WriteInfo",
]

_TS = re.compile(r"^(\d{4}-\d{2}-\d{2})[Tt ](\d{2}:\d{2}:\d{2})(?:\.(\d+))?\s*([Zz]|[+-]\d{2}:?\d{2})?$")


def parse_time(value: Any) -> datetime | None:
    """Parse an RFC 3339 timestamp with up to nanosecond precision.

    Returns ``None`` for a missing or unparseable value rather than raising:
    a timestamp must never make a response unusable.
    """
    if not isinstance(value, str):
        return None
    m = _TS.match(value.strip())
    if not m:
        return None
    date, clock, frac, zone = m.groups()
    micro = int((frac or "0")[:6].ljust(6, "0"))
    try:
        dt = datetime.strptime(f"{date}T{clock}", "%Y-%m-%dT%H:%M:%S").replace(microsecond=micro)
    except ValueError:
        return None
    if zone is None or zone in ("Z", "z"):
        return dt.replace(tzinfo=timezone.utc)
    try:
        return datetime.fromisoformat(dt.isoformat() + (zone if ":" in zone else zone[:3] + ":" + zone[3:]))
    except ValueError:
        return None


def _str(d: Mapping[str, Any], key: str, default: str = "") -> str:
    v = d.get(key)
    return v if isinstance(v, str) else default


def _opt_str(d: Mapping[str, Any], key: str) -> str | None:
    v = d.get(key)
    return v if isinstance(v, str) and v else None


def _int(d: Mapping[str, Any], key: str, default: int = 0) -> int:
    v = d.get(key)
    if isinstance(v, bool):
        return default
    if isinstance(v, int):
        return v
    if isinstance(v, float):
        return int(v)
    return default


def _str_list(v: Any) -> list[str]:
    if isinstance(v, Sequence) and not isinstance(v, (str, bytes)):
        return [x for x in v if isinstance(x, str)]
    return []


def _str_map(v: Any) -> dict[str, str]:
    if isinstance(v, Mapping):
        return {str(k): str(x) for k, x in v.items()}
    return {}


@dataclass(frozen=True)
class NetworkPolicy:
    """Egress policy of a sandbox.

    ``mode`` is ``"none"`` (no egress), ``"allowlist"`` (only the listed
    destinations) or ``"full"`` (any public destination); ``None`` means the
    template's mode. Deny always wins over allow.
    """

    mode: str | None = None
    allow_domains: Sequence[str] = ()
    allow_cidrs: Sequence[str] = ()
    deny_cidrs: Sequence[str] = ()

    def to_json(self) -> dict[str, Any]:
        out: dict[str, Any] = {}
        if self.mode:
            out["mode"] = self.mode
        if self.allow_domains:
            out["allow_domains"] = list(self.allow_domains)
        if self.allow_cidrs:
            out["allow_cidrs"] = list(self.allow_cidrs)
        if self.deny_cidrs:
            out["deny_cidrs"] = list(self.deny_cidrs)
        return out

    @classmethod
    def from_json(cls, d: Mapping[str, Any]) -> NetworkPolicy:
        return cls(
            mode=_opt_str(d, "mode"),
            allow_domains=tuple(_str_list(d.get("allow_domains"))),
            allow_cidrs=tuple(_str_list(d.get("allow_cidrs"))),
            deny_cidrs=tuple(_str_list(d.get("deny_cidrs"))),
        )

    @classmethod
    def coerce(cls, value: NetworkPolicy | Mapping[str, Any] | str) -> NetworkPolicy:
        """Accept a policy, a mapping with the same keys, or just a mode string."""
        if isinstance(value, NetworkPolicy):
            return value
        if isinstance(value, str):
            return cls(mode=value)
        unknown = set(value) - {"mode", "allow_domains", "allow_cidrs", "deny_cidrs"}
        if unknown:
            raise ValueError(f"unknown network policy keys: {sorted(unknown)}")
        return cls.from_json(value)


@dataclass(frozen=True)
class SandboxInfo:
    """A sandbox as the control plane describes it."""

    sandbox_id: str
    template_id: str
    state: str
    template_version: int = 0
    reason: str | None = None
    created_at: datetime | None = None
    started_at: datetime | None = None
    last_activity_at: datetime | None = None
    expires_at: datetime | None = None
    network: NetworkPolicy | None = None
    metadata: Mapping[str, str] = field(default_factory=lambda: dict[str, str]())
    raw: Mapping[str, Any] = field(default_factory=lambda: dict[str, Any](), repr=False, compare=False)

    @property
    def end_at(self) -> datetime | None:
        """E2B's name for :attr:`expires_at`."""
        return self.expires_at

    @property
    def is_running(self) -> bool:
        return self.state == "running"

    @classmethod
    def from_json(cls, d: Mapping[str, Any]) -> SandboxInfo:
        net = d.get("network")
        # The access token is deliberately not kept in ``raw``.
        raw = {k: v for k, v in d.items() if k != "access_token"}
        return cls(
            sandbox_id=_str(d, "id"),
            template_id=_str(d, "template_id"),
            template_version=_int(d, "template_version"),
            state=_str(d, "state", "unknown"),
            reason=_opt_str(d, "reason"),
            created_at=parse_time(d.get("created_at")),
            started_at=parse_time(d.get("started_at")),
            last_activity_at=parse_time(d.get("last_activity_at")),
            expires_at=parse_time(d.get("expires_at")),
            network=NetworkPolicy.from_json(net) if isinstance(net, Mapping) else None,
            metadata=_str_map(d.get("metadata")),
            raw=raw,
        )


@dataclass(frozen=True)
class SandboxEvent:
    """One entry of a sandbox's event log (state changes and command records)."""

    id: int
    type: str
    at: datetime | None = None
    from_state: str | None = None
    to_state: str | None = None
    reason: str | None = None
    data: Mapping[str, Any] = field(default_factory=lambda: dict[str, Any]())
    raw: Mapping[str, Any] = field(default_factory=lambda: dict[str, Any](), repr=False, compare=False)

    @classmethod
    def from_json(cls, d: Mapping[str, Any]) -> SandboxEvent:
        data = d.get("data")
        return cls(
            id=_int(d, "id"),
            type=_str(d, "type"),
            at=parse_time(d.get("at")),
            from_state=_opt_str(d, "from_state"),
            to_state=_opt_str(d, "to_state"),
            reason=_opt_str(d, "reason"),
            data=dict(data) if isinstance(data, Mapping) else {},
            raw=dict(d),
        )


@dataclass(frozen=True)
class SandboxStats:
    """Resource counters of a live sandbox, read from its cgroup."""

    memory_bytes: int = 0
    oom_kills: int = 0
    cpu_usage_usec: int = 0
    cpu_throttled_periods: int = 0
    cpu_throttled_usec: int = 0
    pids_current: int = 0
    pids_limit_hits: int = 0
    memory_high_hits: int = 0
    raw: Mapping[str, Any] = field(default_factory=lambda: dict[str, Any](), repr=False, compare=False)

    @classmethod
    def from_json(cls, d: Mapping[str, Any]) -> SandboxStats:
        return cls(
            memory_bytes=_int(d, "memory_bytes"),
            oom_kills=_int(d, "oom_kills"),
            cpu_usage_usec=_int(d, "cpu_usage_usec"),
            cpu_throttled_periods=_int(d, "cpu_throttled_periods"),
            cpu_throttled_usec=_int(d, "cpu_throttled_usec"),
            pids_current=_int(d, "pids_current"),
            pids_limit_hits=_int(d, "pids_limit_hits"),
            memory_high_hits=_int(d, "memory_high_hits"),
            raw=dict(d),
        )


@dataclass(frozen=True)
class TemplateInfo:
    """A template a tenant may create sandboxes from."""

    template_id: str
    version: int = 0
    image: str = ""
    runtime: str = ""
    network: str = ""
    idle_timeout_ms: int = 0
    max_lifetime_ms: int = 0
    resources: Mapping[str, Any] = field(default_factory=lambda: dict[str, Any]())
    raw: Mapping[str, Any] = field(default_factory=lambda: dict[str, Any](), repr=False, compare=False)

    @classmethod
    def from_json(cls, d: Mapping[str, Any]) -> TemplateInfo:
        res = d.get("resources")
        return cls(
            template_id=_str(d, "id"),
            version=_int(d, "version"),
            image=_str(d, "image"),
            runtime=_str(d, "runtime"),
            network=_str(d, "network"),
            idle_timeout_ms=_int(d, "idle_timeout_ms"),
            max_lifetime_ms=_int(d, "max_lifetime_ms"),
            resources=dict(res) if isinstance(res, Mapping) else {},
            raw=dict(d),
        )


@dataclass(frozen=True)
class ExitStatus:
    """How a command ended.

    ``reason`` is ``exited``, ``killed`` (by a signal), ``timeout`` (the
    per-command limit) or ``oom``. ``exit_code`` is 128 + the signal number
    when the command was ended by a signal.
    """

    exit_code: int
    reason: str = "exited"
    signal: str | None = None

    @classmethod
    def from_json(cls, d: Mapping[str, Any]) -> ExitStatus:
        return cls(
            exit_code=_int(d, "exit_code", -1),
            reason=_str(d, "reason", "exited"),
            signal=_opt_str(d, "signal"),
        )


@dataclass(frozen=True)
class CommandInfo:
    """A command (process) in a sandbox. ``pid`` is an opaque string, not a kernel pid."""

    pid: str
    cmd: str = ""
    args: Sequence[str] = ()
    tag: str | None = None
    pty: bool = False
    started_at: datetime | None = None
    exit: ExitStatus | None = None
    next_offset: int = 0
    raw: Mapping[str, Any] = field(default_factory=lambda: dict[str, Any](), repr=False, compare=False)

    @property
    def running(self) -> bool:
        return self.exit is None

    @classmethod
    def from_json(cls, d: Mapping[str, Any]) -> CommandInfo:
        ex = d.get("exit")
        return cls(
            pid=_str(d, "pid"),
            cmd=_str(d, "cmd"),
            args=tuple(_str_list(d.get("args"))),
            tag=_opt_str(d, "tag"),
            pty=bool(d.get("pty")),
            started_at=parse_time(d.get("started_at")),
            exit=ExitStatus.from_json(ex) if isinstance(ex, Mapping) else None,
            next_offset=_int(d, "next_offset"),
            raw=dict(d),
        )


@dataclass(frozen=True)
class CommandResult:
    """The outcome of a finished command.

    ``truncated`` is true when part of the output was lost: the client fell
    behind the sandbox's 1 MiB replay buffer, or attached too late.
    ``dropped_bytes`` says how much. Output is decoded as UTF-8 with
    replacement characters; the raw bytes are in ``stdout_bytes`` and
    ``stderr_bytes``.
    """

    pid: str
    exit_code: int
    reason: str = "exited"
    signal: str | None = None
    stdout: str = ""
    stderr: str = ""
    stdout_bytes: bytes = field(default=b"", repr=False)
    stderr_bytes: bytes = field(default=b"", repr=False)
    truncated: bool = False
    dropped_bytes: int = 0

    @property
    def error(self) -> str | None:
        """E2B-style: a short description when the command did not succeed."""
        if self.exit_code == 0 and self.reason == "exited":
            return None
        return self.reason if self.reason != "exited" else f"exit status {self.exit_code}"

    @property
    def ok(self) -> bool:
        return self.exit_code == 0


@dataclass(frozen=True)
class StdoutEvent:
    """A chunk of stdout at ``offset`` of the command's output stream."""

    data: bytes
    offset: int

    @property
    def text(self) -> str:
        return self.data.decode("utf-8", "replace")


@dataclass(frozen=True)
class StderrEvent:
    """A chunk of stderr."""

    data: bytes
    offset: int

    @property
    def text(self) -> str:
        return self.data.decode("utf-8", "replace")


@dataclass(frozen=True)
class PtyOutputEvent:
    """A chunk of PTY output (stdout and stderr merged by the terminal)."""

    data: bytes
    offset: int

    @property
    def text(self) -> str:
        return self.data.decode("utf-8", "replace")


@dataclass(frozen=True)
class TruncatedEvent:
    """``dropped`` bytes of output before ``offset`` were lost and will not be delivered."""

    offset: int
    dropped: int


@dataclass(frozen=True)
class ExitEvent:
    """The command ended; always the last event."""

    exit: ExitStatus
    offset: int


CommandEvent = Union[StdoutEvent, StderrEvent, PtyOutputEvent, TruncatedEvent, ExitEvent]


@dataclass(frozen=True)
class EntryInfo:
    """A directory entry in the sandbox."""

    name: str
    path: str
    size: int = 0
    mode: str = ""
    is_dir: bool = False
    modified_time: datetime | None = None
    raw: Mapping[str, Any] = field(default_factory=lambda: dict[str, Any](), repr=False, compare=False)

    @property
    def type(self) -> str:
        """E2B-style: ``"dir"`` or ``"file"``."""
        return "dir" if self.is_dir else "file"

    @classmethod
    def from_json(cls, d: Mapping[str, Any]) -> EntryInfo:
        mode = d.get("mode")
        return cls(
            name=_str(d, "name"),
            path=_str(d, "path"),
            size=_int(d, "size"),
            mode=mode if isinstance(mode, str) else (format(mode, "04o") if isinstance(mode, int) else ""),
            is_dir=bool(d.get("is_dir")),
            modified_time=parse_time(d.get("mod_time")),
            raw=dict(d),
        )


@dataclass(frozen=True)
class WriteInfo:
    """A file written by ``files.write``."""

    path: str
    size: int


@dataclass(frozen=True)
class WriteEntry:
    """One file for ``files.write_files``."""

    path: str
    data: str | bytes | IO[bytes] | IO[str]
    mode: str | int | None = None
