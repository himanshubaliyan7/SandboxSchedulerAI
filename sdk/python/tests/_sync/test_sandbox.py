# Generated from tests/_async/test_sandbox.py by scripts/unasync.py. Do not edit.
"""Sandbox lifecycle, credentials and the control plane."""

from __future__ import annotations

from datetime import datetime, timezone

import httpx
import pytest

from sbx import (
    AuthenticationError,
    ConfigurationError,
    ConflictError,
    InvalidArgumentError,
    NetworkPolicy,
    Sandbox,
    SandboxNotFoundError,
    SandboxNotRunningError,
    TemplateNotFoundError,
    __version__,
)

from ..fake import API_KEY, OTHER_KEY, FakeServer, FakeWSServer
from .helpers import BASE_URL


def test_create_sends_the_documented_request(server: FakeServer) -> None:
    sb = Sandbox.create(
        "python",
        timeout=90,
        envs={"A": "1"},
        metadata={"job": "42"},
        network=NetworkPolicy(mode="allowlist", allow_domains=["pypi.org"]),
        api_key=API_KEY,
        base_url=BASE_URL + "/",
        transport=server.transport(),
    )
    request = server.seen("POST", r"^/v1/sandboxes$")[0]
    assert httpx.Response(200, content=request.content).json() == {
        "template_id": "python",
        "timeout_ms": 90_000,
        "env": {"A": "1"},
        "metadata": {"job": "42"},
        "network": {"mode": "allowlist", "allow_domains": ["pypi.org"]},
    }
    assert request.headers["x-api-key"] == API_KEY
    assert "x-access-token" not in request.headers
    assert request.headers["user-agent"].startswith(f"sbx-python/{__version__} ")
    assert sb.sandbox_id.startswith("sb-")
    assert sb.access_token is not None and sb.access_token.startswith("sbt_")
    assert sb.template_id == "python"
    info = sb.info
    assert info is not None
    assert info.state == "running" and info.is_running
    assert info.metadata == {"job": "42"}
    assert info.network == NetworkPolicy(mode="allowlist", allow_domains=("pypi.org",))
    # Nanosecond timestamps are parsed (truncated to microseconds).
    assert info.created_at == datetime(2026, 10, 10, 8, 0, 0, 123456, tzinfo=timezone.utc)
    assert info.end_at == datetime(2026, 10, 10, 9, 0, tzinfo=timezone.utc)
    # Unknown fields are kept in raw, and the token is not.
    assert info.raw["field_from_the_future"] == 1
    assert "access_token" not in info.raw
    sb.close()


def test_network_accepts_a_mode_string_and_a_mapping(server: FakeServer) -> None:
    for network, wanted in (
        ("none", {"mode": "none"}),
        ({"mode": "allowlist", "allow_cidrs": ["1.1.1.1/32"]}, None),
    ):
        sb = Sandbox.create("python", network=network, api_key=API_KEY, transport=server.transport())
        body = httpx.Response(200, content=server.seen("POST", r"^/v1/sandboxes$")[-1].content).json()
        assert body["network"] == (wanted or network)
        sb.close()
    with pytest.raises(ValueError, match="unknown network policy keys"):
        Sandbox.create("python", network={"mood": "none"}, api_key=API_KEY, transport=server.transport())


def test_environment_fallbacks(server: FakeServer, monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setenv("SBX_API_KEY", API_KEY)
    monkeypatch.setenv("SBX_BASE_URL", "http://from-env.test:9000")
    sb = Sandbox.create("python", transport=server.transport())
    request = server.requests[-1]
    assert request.url.host == "from-env.test" and request.url.port == 9000
    assert request.headers["x-api-key"] == API_KEY
    sb.close()


def test_missing_api_key_is_a_configuration_error(
    server: FakeServer, monkeypatch: pytest.MonkeyPatch
) -> None:
    monkeypatch.delenv("SBX_API_KEY", raising=False)
    with pytest.raises(ConfigurationError, match="API key"):
        Sandbox.create("python", transport=server.transport())
    assert server.requests == []
    with pytest.raises(ConfigurationError):
        Sandbox.connect("sb-1", transport=server.transport())
    with pytest.raises(ConfigurationError, match="http"):
        Sandbox.create("python", api_key=API_KEY, base_url="ftp://x", transport=server.transport())


def test_wrong_key_and_unknown_template(server: FakeServer) -> None:
    with pytest.raises(AuthenticationError) as e:
        Sandbox.create("python", api_key="sbx_wrong", transport=server.transport())
    assert e.value.status_code == 401 and e.value.code == "unauthorized" and e.value.request_id == "req-test"
    with pytest.raises(TemplateNotFoundError):
        Sandbox.create("nope", api_key=API_KEY, transport=server.transport())
    with pytest.raises(InvalidArgumentError) as e2:
        Sandbox.create("python", timeout=7200, api_key=API_KEY, transport=server.transport())
    assert e2.value.code == "invalid_timeout" and e2.value.details == {"max_timeout_ms": 3_600_000}


def test_data_plane_uses_the_access_token_only(sandbox: Sandbox, server: FakeServer) -> None:
    sandbox.commands.run("true")
    sandbox.files.write("a.txt", "x")
    sandbox.get_info()
    for request in server.requests[1:]:
        if "/commands" in request.url.path or "/files" in request.url.path:
            assert request.headers["x-access-token"] == sandbox.access_token
            assert "x-api-key" not in request.headers
        else:
            assert request.headers["x-api-key"] == API_KEY
            assert "x-access-token" not in request.headers
        assert "token" not in request.url.params


def test_secrets_are_not_in_repr(sandbox: Sandbox) -> None:
    token = sandbox.access_token
    assert token
    for obj in (sandbox, sandbox.config, sandbox.info, sandbox.commands, sandbox._api):
        assert token not in repr(obj)
        assert API_KEY not in repr(obj)


def test_connect_with_id_and_token_only(
    sandbox: Sandbox, server: FakeServer, monkeypatch: pytest.MonkeyPatch
) -> None:
    monkeypatch.delenv("SBX_API_KEY", raising=False)
    assert sandbox.access_token
    other = Sandbox.connect(
        sandbox.sandbox_id, access_token=sandbox.access_token, base_url=BASE_URL, transport=server.transport()
    )
    before = len(server.requests)
    assert (other.commands.run("true")).exit_code == 0
    assert "x-api-key" not in server.requests[before].headers
    assert other.is_running()
    assert other.info is None and other.template_id is None
    with pytest.raises(ConfigurationError, match="API key"):
        other.get_info()
    with pytest.raises(ConfigurationError):
        other.kill()
    # Leaving the context does not (and cannot) kill it.
    with other:
        pass
    assert server.sandbox().state == "running"
    with pytest.raises(AuthenticationError):
        Sandbox.connect(
            sandbox.sandbox_id, access_token="sbt_wrong", base_url=BASE_URL, transport=server.transport()
        )


def test_connect_with_api_key(sandbox: Sandbox, server: FakeServer) -> None:
    other = Sandbox.connect(sandbox.sandbox_id, api_key=API_KEY, transport=server.transport())
    assert other.info is not None and other.info.sandbox_id == sandbox.sandbox_id
    # Without the token the API key is used on the data plane too.
    other.commands.list()
    assert server.requests[-1].headers["x-api-key"] == API_KEY
    with pytest.raises(SandboxNotFoundError):
        Sandbox.connect("sb-missing", api_key=API_KEY, transport=server.transport())


def test_another_tenant_sees_not_found(sandbox: Sandbox, server: FakeServer) -> None:
    with pytest.raises(SandboxNotFoundError) as e:
        Sandbox.connect(sandbox.sandbox_id, api_key=OTHER_KEY, transport=server.transport())
    assert e.value.status_code == 404 and e.value.code == "not_found"
    assert Sandbox.list(api_key=OTHER_KEY, transport=server.transport()) == []


def test_list_and_filters(server: FakeServer) -> None:
    a = Sandbox.create("python", metadata={"job": "1"}, api_key=API_KEY, transport=server.transport())
    b = Sandbox.create("python", metadata={"job": "2"}, api_key=API_KEY, transport=server.transport())
    listed = Sandbox.list(api_key=API_KEY, transport=server.transport())
    assert [s.sandbox_id for s in listed] == [b.sandbox_id, a.sandbox_id]
    only = Sandbox.list(
        metadata={"job": "1"}, template="python", limit=5, state=["running", "assigned"],
        api_key=API_KEY, transport=server.transport(),
    )  # fmt: skip
    assert [s.sandbox_id for s in only] == [a.sandbox_id]
    params = server.requests[-1].url.params
    assert params.get_list("metadata") == ["job=1"]
    assert (
        params["template_id"] == "python" and params["limit"] == "5" and params["state"] == "running,assigned"
    )
    b.kill()
    everything = Sandbox.list(state="all", api_key=API_KEY, transport=server.transport())
    assert {s.state for s in everything} == {"running", "destroyed"}
    templates = Sandbox.list_templates(api_key=API_KEY, transport=server.transport())
    assert templates[0].template_id == "python" and templates[0].max_lifetime_ms == 3_600_000
    assert templates[0].resources["memory_bytes"] == 268435456


def test_info_running_kill(sandbox: Sandbox, server: FakeServer) -> None:
    assert (sandbox.get_info()).state == "running"
    assert sandbox.is_running()
    assert sandbox.kill() is True
    assert sandbox.is_running() is False
    info = sandbox.get_info()
    assert info.state == "destroyed" and info.reason == "released"
    assert sandbox.kill() is True  # idempotent on the server
    with pytest.raises(SandboxNotRunningError) as e:
        sandbox.commands.run("true")
    assert e.value.state == "destroyed" and e.value.reason == "released" and e.value.status_code == 409
    del server.sandboxes[sandbox.sandbox_id]
    assert sandbox.kill() is False
    assert sandbox.is_running() is False


def test_kill_tolerates_an_asynchronous_delete(sandbox: Sandbox, server: FakeServer) -> None:
    server.delete_status = 202
    assert sandbox.kill() is True
    # The sandbox is still on its way out, in a state this SDK has no name for.
    assert (sandbox.get_info()).state == "recycling"
    assert sandbox.is_running() is False


def test_unknown_state_and_reason_are_tolerated(sandbox: Sandbox, server: FakeServer) -> None:
    fake = server.sandbox()
    fake.state, fake.reason = "hibernating", "cosmic_rays"
    info = sandbox.get_info()
    assert (info.state, info.reason, info.is_running) == ("hibernating", "cosmic_rays", False)
    assert sandbox.is_running() is False
    with pytest.raises(SandboxNotRunningError) as e:
        sandbox.commands.list()
    assert e.value.state == "hibernating" and e.value.reason == "cosmic_rays"


def test_set_timeout(sandbox: Sandbox, server: FakeServer) -> None:
    info = sandbox.set_timeout(30)
    request = server.seen("POST", r"/timeout$")[0]
    assert httpx.Response(200, content=request.content).json() == {"timeout_ms": 30_000}
    assert info.expires_at == datetime(2026, 10, 10, 8, 0, 30, tzinfo=timezone.utc)
    sandbox.set_timeout(0.0004)  # rounds up to the smallest value the API accepts
    assert httpx.Response(200, content=server.requests[-1].content).json() == {"timeout_ms": 1}
    with pytest.raises(ValueError):
        sandbox.set_timeout(0)
    with pytest.raises(ValueError):
        sandbox.set_timeout(-1)


def test_update_network(sandbox: Sandbox, server: FakeServer) -> None:
    info = sandbox.update_network(NetworkPolicy(mode="allowlist", allow_domains=["example.com"]))
    assert info.network == NetworkPolicy(mode="allowlist", allow_domains=("example.com",))
    assert server.sandbox().network == {"mode": "allowlist", "allow_domains": ["example.com"]}
    info = sandbox.update_network("none")
    assert info.network is not None and info.network.mode == "none"
    locked = Sandbox.create("python", network="none", api_key=API_KEY, transport=server.transport())
    with pytest.raises(ConflictError) as e:
        locked.update_network("allowlist")
    assert e.value.code == "network_not_attached"


def test_events_and_stats(sandbox: Sandbox, server: FakeServer) -> None:
    sandbox.commands.run("true")
    events = sandbox.events()
    assert [e.type for e in events] == ["claimed", "transition", "command_finished"]
    assert (events[1].from_state, events[1].to_state) == ("assigned", "running")
    assert events[2].data["exit_code"] == 0 and events[2].at is not None
    finished = sandbox.events(type="command_finished")
    assert [e.type for e in finished] == ["command_finished"]
    assert server.requests[-1].url.params["type"] == "command_finished"
    stats = sandbox.stats()
    assert (stats.memory_bytes, stats.pids_current, stats.cpu_usage_usec) == (2_600_000, 2, 12345)


def test_context_manager_kills_on_exit(server: FakeServer, ws: FakeWSServer) -> None:
    with Sandbox.create("python", api_key=API_KEY, transport=server.transport()) as sb:
        assert sb.is_running()
    assert server.sandbox().state == "destroyed"
    assert len(server.seen("DELETE", r"^/v1/sandboxes/sb-")) == 1
    # The exception of the body wins over a failing kill.
    server.inject(
        "DELETE",
        r"^/v1/sandboxes/",
        httpx.Response(500, json={"error": {"code": "internal", "message": "x"}}),
    )
    with pytest.raises(KeyError):
        with Sandbox.create("python", api_key=API_KEY, transport=server.transport()):
            raise KeyError("from the body")


def test_a_caller_supplied_http_client_is_used_and_left_open(server: FakeServer) -> None:
    with httpx.Client(transport=server.transport(), headers={"X-Trace": "t1"}) as client:
        sb = Sandbox.create("python", api_key=API_KEY, http_client=client, headers={"X-Extra": "e1"})
        sb.close()
        assert not client.is_closed
        assert server.requests[-1].headers["x-trace"] == "t1"
        assert server.requests[-1].headers["x-extra"] == "e1"
