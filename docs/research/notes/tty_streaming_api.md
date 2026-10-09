# Client-facing layer of a code-execution sandbox: TTY streaming, wire protocol, API/SDK surface, gateway stack

Research date: 2026-10-09. Every cited finding was read from the linked page on that date unless marked otherwise. Items marked "(unverified, from background knowledge)" appear only under Inferences or Gaps and must not be treated as sourced.

## 1. How to attach a PTY to a process in a Podman container and stream it (Podman API attach/exec, conmon, in-sandbox agent, host-side PTY)

### Takeaway
Podman's REST API can do the job for a single attached stream per exec (HTTP hijack, Docker-compatible 8-byte multiplexing, raw bytes when TTY is on, resize as a separate POST), but it has no documented reattach or replay for exec sessions. Every hosted sandbox product examined (E2B envd, Daytona toolbox, Cloudflare sandbox-shim, OpenHands action server) instead runs a daemon inside the sandbox that owns the PTY and exposes list/connect-by-id, which is what makes reconnect and many concurrent commands workable.

### Cited Findings

Podman REST API (attach)
- Libpod attach is `POST /libpod/containers/{name}/attach`; compat is `POST /containers/{name}/attach`. Query parameters: `detachKeys`, `logs`, `stream` (default true), `stdin`, `stdout`, `stderr`. Response `101` means the connection was hijacked. — [podman register_containers.go](https://raw.githubusercontent.com/containers/podman/main/pkg/api/server/register_containers.go)
- The endpoint "hijacks the HTTP connection to transport stdin, stdout, and stderr on the same socket". Clients may send `Upgrade: tcp` and `Connection: Upgrade` as a proxy hint; the service answers `101 UPGRADED` with `Content-Type: application/vnd.docker.raw-stream`, after which the TCP connection is raw and bidirectional. — [podman register_containers.go](https://raw.githubusercontent.com/containers/podman/main/pkg/api/server/register_containers.go)
- Without a TTY the content type is `application/vnd.docker.multiplexed-stream` (introduced in Podman v4.7.0; raw-stream was always reported before). Each frame has an 8-byte header `[8]byte{STREAM_TYPE, 0, 0, 0, SIZE1, SIZE2, SIZE3, SIZE4}`; stream type 0 = stdin (echoed to stdout), 1 = stdout, 2 = stderr; size is a big-endian uint32 payload length. — [podman register_containers.go](https://raw.githubusercontent.com/containers/podman/main/pkg/api/server/register_containers.go)
- With TTY enabled "the stream is not multiplexed": the socket carries raw PTY output and client stdin, so stdout and stderr cannot be separated. — [podman register_containers.go](https://raw.githubusercontent.com/containers/podman/main/pkg/api/server/register_containers.go)
- Container TTY resize is `POST /libpod/containers/{name}/resize` (and compat `/containers/{name}/resize`) with query `h`, `w` (characters) and `running` (ignore not-running errors). — [podman register_containers.go](https://raw.githubusercontent.com/containers/podman/main/pkg/api/server/register_containers.go)

Podman REST API (exec)
- Exec create: `POST /libpod/containers/{name}/exec` (compat `/containers/{name}/exec`). Body: `AttachStdin`, `AttachStdout`, `AttachStderr`, `DetachKeys`, `Tty`, `Env` (`VAR=value` array), `Cmd`, `Privileged`, `User`, `WorkingDir`. Returns 201; 409 if container paused. Exec sessions are removed "5 minutes after they exit". — [podman register_exec.go](https://raw.githubusercontent.com/containers/podman/main/pkg/api/server/register_exec.go)
- Exec start: `POST /libpod/exec/{id}/start` (compat `/exec/{id}/start`). Libpod body has `Detach`, `Tty`, and initial size `h`/`w` (only valid with Tty true). "The stream format is the same as the attach endpoint." In the compat endpoint `Detach` is "Not presently supported" and `Tty` is "Presently ignored". — [podman register_exec.go](https://raw.githubusercontent.com/containers/podman/main/pkg/api/server/register_exec.go)
- Exec resize: `POST /libpod/exec/{id}/resize` (compat `/exec/{id}/resize`) with `h`, `w`, `running`; "only works if tty was specified as part of creating and starting the exec instance". Exec inspect: `GET /libpod/exec/{id}/json`. — [podman register_exec.go](https://raw.githubusercontent.com/containers/podman/main/pkg/api/server/register_exec.go)
- `podman exec -d` starts the session without attaching, prints the exec session ID, and the session is removed when the command finishes. — [podman-exec(1), v5.8.2](https://docs.podman.io/en/v5.8.2/markdown/podman-exec.1.html)

Docker Engine API (for comparison; Podman's compat layer mirrors it)
- Docker has a WebSocket attach variant, `GET /containers/{id}/attach/ws`, with query `detachKeys`, `logs`, `stream`, `stdin`, `stdout`, `stderr` (all default false); responses 101 / 200 ("no upgrade header found") / 400 / 404 / 500. The operation has no stream-format description. — [moby swagger.yaml](https://raw.githubusercontent.com/moby/moby/master/api/swagger.yaml)
- Docker's ContainerAttach uses the same rule: `application/vnd.docker.multiplexed-stream` when TTY is disabled, unmultiplexed raw PTY data when TTY is enabled. — [moby swagger.yaml](https://raw.githubusercontent.com/moby/moby/master/api/swagger.yaml)

conmon's role
- Podman's attach code (`libpod/oci_conmon_attach_common.go`) attaches to an exec session through conmon; the attach fd first carries a nonce from conmon confirming the console socket is ready and later the exit code of the exec process or an error. — [search summary of oci_conmon_attach_common.go](https://gitverse.ru/balmaster/podman/content/main/libpod/oci_conmon_attach_common.go) (secondary mirror; not read line by line)

In-sandbox agent daemons (what the products do)
- E2B `envd` is "a daemon that runs inside a sandbox" that lets the SDK interact with it; the repo is Go (`go.mod`, `main.go`), specs live under `spec/` and generate server stubs, version is tracked in `pkg/version.go`. — [e2b-dev/infra packages/envd](https://github.com/e2b-dev/infra/tree/main/packages/envd)
- envd's `process.Process` service exposes `List`, `Connect` (attach to an existing process and stream events), `Start` (launch and stream events), `Update` (change PTY size), `StreamInput` (client-streamed ordered input), `SendInput`, `SendSignal`, and `CloseStdin` (non-PTY only; PTY processes get Ctrl+D 0x04 instead). Processes are selected by PID or by a caller-supplied tag. — [envd process.proto](https://raw.githubusercontent.com/e2b-dev/infra/main/packages/envd/spec/process/process.proto)
- Daytona's Toolbox API is an HTTP service that starts inside each sandbox and exposes process, file system, Git and LSP operations; the process group covers command execution, PTY sessions and shell sessions. — [Daytona toolbox overview (mintlify mirror)](https://www.mintlify.com/daytonaio/daytona/api/toolbox/overview)
- Daytona PTY endpoints on the toolbox: `POST /process/pty` (create), `GET /process/pty/{sessionId}/connect` (WebSocket), `GET /process/pty` (list), `GET /process/pty/{sessionId}`, `POST /process/pty/{sessionId}/resize`, `DELETE /process/pty/{sessionId}`. Parameters: `id`, `cwd`, `envs`, `cols`, `rows`. Disconnecting closes "the WebSocket without killing the shell" and a session can be reattached with connect. — [Daytona PTY docs](https://www.daytona.io/docs/en/pty)
- Cloudflare Sandbox SDK: a sandbox is a Durable Object plus the Container it starts; a Linux helper `sandbox-shim` runs inside the container and exchanges frames with the `@cloudflare/sandbox` package (protocol in `docs/shim-protocol.md`). The repo contains `Cargo.toml` and `tsconfig.json`. — [cloudflare/sandbox-sdk](https://github.com/cloudflare/sandbox-sdk)
- OpenHands: an action execution server inside the Docker container starts an `ActionExecutor` that initializes a bash shell and plugins; the backend sends actions and receives observations over a RESTful API; a Jupyter plugin runs a Kernel Gateway in the container for IPython cells. — [OpenHands runtime architecture](https://docs.openhands.dev/openhands/usage/architecture/runtime)
- microsandbox takes the opposite approach: the SDK boots a microVM as a child process with "No setup server. No long-running daemon."; execution is `sandbox.exec("python", [...])`. — [microsandbox repo](https://github.com/zerocore-ai/microsandbox)

### Inferences
- Podman API exec is adequate for one-shot commands where the gateway holds the hijacked connection for the command's whole life. It is a poor fit for interactive sessions: the documented surface has create/start/resize/inspect but no "attach to a running exec session" and no output replay, and exec sessions are garbage collected 5 minutes after exit. A gateway restart or client drop therefore loses the stream.
- Container-level attach (to PID 1) can be reattached "multiple times" per the compat description, and `logs=true` replays the container log first, but that gives exactly one stream per container, so it cannot serve multiple concurrent commands.
- With TTY on, both Podman and Docker merge stdout and stderr. An API that promises separate stdout/stderr for one-shot commands must run those without a PTY (pipes), and use a PTY only for interactive sessions. envd encodes exactly this split: its data event is one of stdout, stderr or pty.
- An in-sandbox agent that owns the PTY master (or pipes) gives: process identity independent of any client connection, a registry (list/connect by pid or tag), per-process ring buffers for replay, signals, and stdin close. The cost is a binary injected into every image, a listening port inside the sandbox that must be authenticated, and a version-compatibility contract between agent and SDK (E2B gates features on envd version).
- Host-side PTY allocation (gateway opens the PTY and uses `podman exec -t` or nsenter as the slave's child) keeps the image clean but ties process lifetime to the host-side process holding the master, makes rootless namespace entry the gateway's problem, and gives no in-sandbox place to buffer output. A reasonable hybrid for a rootless Podman design: use the Podman API only for lifecycle (create/start/stop/inspect) and to launch the agent as PID 1 or a sidecar process, then do all exec/PTY/file traffic through the agent.

### Gaps
- No primary source was found stating whether a detached Podman exec session can be attached later through the REST API. The man page says stdin "blocks until the session is attached later" but does not document how. Needs a test against the target Podman version or a read of `libpod/container_exec.go`.
- The Podman API reference page (docs.podman.io `_static/api.html`) is JavaScript-rendered and returned no text; the findings above come from the swagger comments in the Podman source on `main`, so the current released API version number was not captured.
- Whether Podman implements the Docker `/attach/ws` WebSocket endpoint was not confirmed.
- Modal's in-sandbox mechanism is not publicly documented in the pages read.
- conmon details came from a search summary of a mirror, not from the upstream file.

## 2. Wire protocol design: framing, stdout/stderr vs PTY, resize, stdin, signals, exit codes, heartbeats, backpressure, replay, caps

### Takeaway
Every mature protocol uses a one-byte channel or opcode prefix on binary frames (Kubernetes, ttyd, gotty, Docker) or typed events with a oneof payload (envd, Jupyter); none of them provides replay-on-reconnect or end-to-end flow control out of the box, so those must be designed in explicitly (sequence numbers plus a bounded ring buffer, and ACK-based watermarks as xterm.js recommends).

### Cited Findings

Kubernetes exec/attach WebSocket subprotocol
- Subprotocol names: `channel.k8s.io` (v1), `v2.channel.k8s.io`, `v3.channel.k8s.io` ("adds support for resizing container terminals"), `v4.channel.k8s.io` ("adds support for exit codes"), `v5.channel.k8s.io` ("adds support for a CLOSE signal"). — [apimachinery remotecommand constants.go](https://raw.githubusercontent.com/kubernetes/kubernetes/master/staging/src/k8s.io/apimachinery/pkg/util/remotecommand/constants.go)
- Channel IDs (first byte of each frame): `StreamStdIn`=0, `StreamStdOut`=1, `StreamStdErr`=2, `StreamErr`=3, `StreamResize`=4, `StreamClose`=255. `DefaultStreamCreationTimeout` is 30 seconds. Exit codes travel as a status with `NonZeroExitCodeReason` / `ExitCodeCauseType`. — [apimachinery remotecommand constants.go](https://raw.githubusercontent.com/kubernetes/kubernetes/master/staging/src/k8s.io/apimachinery/pkg/util/remotecommand/constants.go)
- From Kubernetes 1.31, kubectl uses WebSocket instead of SPDY by default for `exec`, `attach`, `cp` and `port-forward`. Reason: SPDY/3.1 was deprecated for eight years, never standardized, and many proxies, gateways and load balancers no longer support it. Handshake: `Upgrade: websocket`, `Sec-Websocket-Protocol: v5.channel.k8s.io`, reply `101 Switching Protocols`; stdin, stdout, stderr, resize and exit codes then share one connection. Feature gates `TranslateStreamCloseWebsocketRequests` (v1.29) and `PortForwardWebsockets` (v1.30), both beta and on by default in 1.31. — [Kubernetes blog, 2024-08-20](https://kubernetes.io/blog/2024/08/20/websockets-transition/)

ttyd
- Client-to-server opcodes (first byte, ASCII): `INPUT '0'`, `RESIZE_TERMINAL '1'`, `PAUSE '2'`, `RESUME '3'`, `JSON_DATA '{'`. Server-to-client: `OUTPUT '0'`, `SET_WINDOW_TITLE '1'`, `SET_PREFERENCES '2'`. — [ttyd server.h](https://raw.githubusercontent.com/tsl0922/ttyd/main/src/server.h)

gotty (maintained fork sorenisanerd/gotty)
- WebSocket subprotocol name `webtty`. Client-to-server: `Input '1'`, `Ping '2'`, `ResizeTerminal '3'`, `SetEncoding '4'`. Server-to-client: `Output '1'`, `Pong '2'`, `SetWindowTitle '3'`, `SetPreferences '4'`, `SetReconnect '5'`, `SetBufferSize '6'`. — [gotty webtty/message_types.go](https://raw.githubusercontent.com/sorenisanerd/gotty/master/webtty/message_types.go)

Docker / Podman attach
- 8-byte header: stream type byte, three zero bytes, big-endian uint32 length; types 0 stdin, 1 stdout, 2 stderr; not used when a TTY is allocated. Resize is out of band (separate HTTP POST), and there is no in-band exit code. — [podman register_containers.go](https://raw.githubusercontent.com/containers/podman/main/pkg/api/server/register_containers.go)

E2B envd (typed events over Connect RPC)
- `ProcessEvent` is one of: `start` (carries PID), `data` (stdout bytes, stderr bytes, or pty bytes), `end` (exit code, exited flag, status string, optional error), `keepalive` (empty heartbeat). Input is `ProcessInput` (stdin bytes or pty bytes); `StreamInputRequest` is a start event with a selector followed by data or keepalive events. `Signal` enum has only SIGTERM (15) and SIGKILL (9). `StartRequest` takes process config (cmd, args, envs, optional cwd), optional PTY (size cols/rows), optional tag, and an optional stdin flag that defaults to true for backward compatibility while newer SDKs set it false. — [envd process.proto](https://raw.githubusercontent.com/e2b-dev/infra/main/packages/envd/spec/process/process.proto)
- E2B SDK streams request a keepalive ping from the sandbox every 50 seconds; a stream "normally stays open for more than an hour" but no single connection is guaranteed to last, so clients must reconnect. Applies to `commands.run()`, `commands.connect()`, `pty.create()`, `pty.connect()` and `files.watchDir()`. Causes of drops: load-balancer backend drain or the public-edge request timeout. — [E2B sandbox lifetime FAQ](https://docs.e2b.dev/faq/sandbox-lifetime)
- E2B PTY: `pty.create({cols, rows, onData, envs, cwd, user, timeoutMs})` runs interactive bash with `TERM=xterm-256color`; default timeout 60 seconds, `timeoutMs: 0` disables it; `sendInput(pid, bytes)`, `resize(pid, {cols, rows})`, `kill(pid)`, `disconnect()` (PTY keeps running), `connect(pid, {onData})`, `wait()` returning `exitCode`. — [E2B PTY docs](https://docs.e2b.dev/docs/sandbox/pty)

Jupyter kernel messaging (model for stateful code execution)
- Protocol spec version 5.5. Channels: Shell (requests/replies), IOPub (broadcast of stream output, results, errors, status), stdin (kernel asks frontend for input), Control (shutdown/interrupt/debug, kept separate so it is not queued behind execution), Heartbeat (raw bytestring echo). — [Jupyter messaging spec](https://jupyter-client.readthedocs.io/en/stable/messaging.html)
- Message = `header` (`msg_id`, `session`, `username`, `date`, `msg_type`, `version`), `parent_header` (copy of the causing request's header, which is how outputs are correlated to a request), `metadata`, `content`, optional binary `buffers`. Wire: routing ids, delimiter `<IDS|MSG>`, HMAC-SHA256 hex signature over the four JSON dicts, then the dicts, then raw buffers. — [Jupyter messaging spec](https://jupyter-client.readthedocs.io/en/stable/messaging.html)
- `execute_request`: `code`, `silent`, `store_history`, `user_expressions`, `allow_stdin`, `stop_on_error`. `execute_reply`: `status` ok/error/aborted, `execution_count`, and on error `ename`, `evalue`, `traceback`. IOPub: `stream` (`name` stdout|stderr, `text`), `display_data` (MIME bundle `data`, `metadata`, `transient.display_id`), `execute_result`, `error`, `status` (`execution_state` busy|idle|starting; busy on request receipt, idle after outputs finish). `interrupt_request` goes on Control. — [Jupyter messaging spec](https://jupyter-client.readthedocs.io/en/stable/messaging.html)

Backpressure and flow control
- xterm.js has a hardcoded 50 MB write buffer; data beyond it is discarded. Typical processing throughput is 5 to 35 MB/s while fast producers can emit several GB/s. — [xterm.js flow control guide](https://xtermjs.org/docs/guides/flowcontrol/)
- Recommended watermark scheme: count pending bytes, pause the PTY above HIGH, resume below LOW; example HIGH=100000, LOW=10000; keep HIGH at or below 500K so keystrokes (Ctrl-C) stay responsive. Test with `yes` then Ctrl-C. — [xterm.js flow control guide](https://xtermjs.org/docs/guides/flowcontrol/)
- Over WebSocket the guide treats the socket as a sink of unknown latency and recommends an application-level ACK: the client sends a custom ACK message when its write callback fires (example: once per 100000 bytes; pause at 5 pending, resume at 2); the server pauses the PTY read when the stop condition is hit and resumes on ACK. ACKs must be marked so they are not treated as terminal data. — [xterm.js flow control guide](https://xtermjs.org/docs/guides/flowcontrol/)
- ttyd implements this idea as explicit `PAUSE '2'` / `RESUME '3'` client messages. — [ttyd server.h](https://raw.githubusercontent.com/tsl0922/ttyd/main/src/server.h)
- The browser WebSocket API has no receive-side backpressure; the only send-side signal is polling `bufferedAmount`. Chrome's WebSocketStream adds stream-based send backpressure. — [Chrome WebSocketStream article](https://developer.chrome.com/docs/capabilities/web-apis/websocketstream) (via search summary; sources dated)

Vercel Sandbox (structured log stream rather than a TTY protocol)
- `command.logs()` is an async generator yielding `{ stream: 'stdout' | 'stderr', data: string }`; it may throw `StreamError` if the sandbox stops while streaming. `command.output('stdout' | 'stderr' | 'both')` returns the accumulated output; `sandbox.getCommand(cmdId)` retrieves a command later; `kill(signal?)` defaults to SIGTERM. — [Vercel Sandbox SDK reference](https://vercel.com/docs/sandbox/sdk-reference)

### Inferences
- Recommended frame design for the WebSocket data plane: binary frames, one-byte channel prefix aligned with Kubernetes (0 stdin, 1 stdout, 2 stderr, 3 status/error JSON, 4 resize JSON), plus extra channels for ping/pong-with-sequence, ACK/flow control, and replay markers. Binary avoids base64 inflation and UTF-8 boundary corruption that JSON text frames cause with raw PTY bytes. Carry control messages (resize, signal, exit, error) as small JSON payloads on their own channels for debuggability.
- Offer two execution modes with different stream semantics: pipe mode (separate stdout and stderr, stdin closable, used for one-shot agent commands) and PTY mode (single merged stream, resize, Ctrl+D for EOF). Both Podman/Docker and envd make this same distinction.
- Exit must be an explicit terminal event carrying exit code, signal, and a reason (exited, killed, timeout, OOM, sandbox stopped), followed by a normal close. Kubernetes needed two protocol revisions (v4, v5) to get exit codes and close right, which argues for designing them in at v1.
- Resize should be in band (Kubernetes channel 4, ttyd, gotty, envd `Update`) rather than a separate HTTP call (Docker/Podman), because an out-of-band resize races with output and needs separate auth and routing.
- Signals: envd exposes only SIGTERM and SIGKILL; an interactive API additionally needs SIGINT (or documented Ctrl+C 0x03 on PTY stdin) and Vercel-style `kill(signal)`.
- Heartbeats: application-level keepalive events at a period below typical 60-second idle timeouts of proxies; E2B uses 50 seconds. Use them in both directions so the agent can also detect dead clients.
- Replay on reconnect is not provided by any examined protocol; E2B's docs are silent on whether output emitted during a disconnect is delivered. A design that wants lossless reconnect needs: a monotonically increasing byte offset or sequence number per stream, a bounded ring buffer in the agent, and a `connect(pid, from_offset)` call that either replays from the offset or returns a "truncated, N bytes dropped" marker.
- Output caps are needed at three layers: per-frame size (bounded chunking, e.g. tens of KB), per-process ring buffer for replay, and per-execution total persisted output with an explicit truncation flag. LLM consumers benefit more from head-plus-tail truncation with a flag than from unbounded output.
- For agent (non-browser) clients the xterm.js numbers still give a useful starting point for watermarks (100 KB high, 10 KB low), but the real requirement is that a slow consumer pauses reads from the PTY master rather than growing gateway memory.
- For the stateful Python interpreter, adopt the Jupyter event taxonomy (stream, result with MIME bundle, error with ename/evalue/traceback, busy/idle status, execution_count) rather than inventing one; E2B and OpenHands both sit on a Jupyter kernel.

### Gaps
- xterm.js attach addon source was not read; its behavior (raw bidirectional binary or text frames with no control channel) is not cited here.
- No vendor documents concrete output size caps or ring buffer sizes; no numbers found.
- Current browser support for WebSocketStream was not confirmed; the search results on it were dated.
- Jupyter Enterprise Gateway's WebSocket bridging of the ZeroMQ channels was not researched.

## 3. Transport choice: WebSocket vs SSE vs gRPC/Connect streaming vs WebTransport, and what the products use

### Takeaway
The products split by use: typed RPC with server streaming over plain HTTP for commands and files (E2B uses Connect RPC; Vercel uses an HTTP log stream; Cloudflare uses HTTP or a multiplexed RPC transport and has deprecated its WebSocket transport), and WebSocket for interactive PTY (Daytona). WebSocket is the right choice for the bidirectional PTY path; server-streaming HTTP is simpler and more proxy-friendly for one-shot command output.

### Cited Findings
- E2B: SDK talks to envd with Connect RPC over HTTP, e.g. POST to a `process.Process` method with `Connect-Protocol-Version` header and `Content-Type: application/connect+json`; `StreamInput` is client-streaming. The host is built from the port and sandbox ID under `e2b.app`. — [E2B API reference: process StreamInput](https://www.e2b.dev/docs/api-reference/process/streaminput) (via search summary)
- E2B streams are long-lived HTTP streams with a 50-second keepalive and SDK-side reconnect via `commands.connect(pid)` / `pty.connect(pid)`; WebSocket connections to a sandbox's public URL traverse the same edge and need their own keepalive and reconnect logic. — [E2B sandbox lifetime FAQ](https://docs.e2b.dev/faq/sandbox-lifetime)
- Daytona: PTY handle manages a WebSocket to `GET /process/pty/{sessionId}/connect`; non-PTY execution and sessions are REST on the toolbox. — [Daytona PTY docs](https://www.daytona.io/docs/en/pty)
- Cloudflare Sandbox: between Durable Object and container, HTTP is the default transport (one request per SDK call); an RPC transport multiplexes all calls over one persistent connection (set `SANDBOX_TRANSPORT` to `"rpc"`) to avoid subrequest limits; the WebSocket transport is deprecated in favor of RPC. — [Cloudflare Sandbox architecture](https://developers.cloudflare.com/sandbox/concepts/architecture/)
- Vercel Sandbox: command output is consumed as an async iterator of `{stream, data}` log records or via Writable `stdout`/`stderr` options; detached commands return a `Command` with `cmdId` for later retrieval. — [Vercel Sandbox SDK reference](https://vercel.com/docs/sandbox/sdk-reference)
- Modal: process handles expose `stdout` (read or line iteration), `returncode`, `poll()`, `wait()`, and `sb.stdin.write()`; `detach()` releases client connections without terminating the sandbox; `Sandbox.from_id` reattaches. An older `pty_info` parameter was typed as a protobuf message (`api_pb2.PTYInfo`), superseded by `pty`. — [Modal sandbox guide](https://modal.com/docs/guide/sandbox); [Modal Sandbox reference](https://modal.com/docs/reference/modal.Sandbox)
- OpenHands: REST request/response of actions and observations, no streaming transport described. — [OpenHands runtime architecture](https://docs.openhands.dev/openhands/usage/architecture/runtime)
- Kubernetes moved from SPDY to WebSocket specifically because non-standard streaming transports break at proxies, gateways and load balancers. — [Kubernetes blog, 2024-08-20](https://kubernetes.io/blog/2024/08/20/websockets-transition/)
- One 2026 comparison lists browser support at about 75% for WebTransport, 97% for SSE and 99%+ for WebSocket, and describes SSE as the default transport for LLM token streaming because it passes proxies and CDNs. — [websocket.org comparisons (2026)](https://websocket.org/comparisons/) (secondary source, via search summary)

### Inferences
- Use a two-transport design: (a) REST/JSON control plane; (b) for execution, server-streaming HTTP (SSE or Connect/NDJSON) for "run command and stream output", and WebSocket for interactive PTY and any case needing concurrent stdin. This matches E2B (HTTP streaming plus separate unary `SendInput`) and Daytona (REST plus WebSocket PTY).
- SSE or Connect server-streaming is the lower-risk default for agent clients: it works with any HTTP client, HTTP/2 multiplexes many streams on one connection, and auth is ordinary headers. Its limits are no client-to-server stream (stdin needs separate POSTs, as envd's `SendInput` does) and text-only SSE framing (binary must be base64 or the stream must be NDJSON/Connect envelopes).
- gRPC proper is awkward at the public edge (HTTP/2 trailers, browser incompatibility); Connect gives the same protobuf schema with plain HTTP/1.1 and HTTP/2 POST semantics, which is presumably why E2B chose it. Modal's protobuf-typed parameters suggest a gRPC client, but that is not confirmed by the pages read.
- WebTransport is not justified in 2026 for this workload: support is incomplete, agent clients are server-side processes, and none of the six products use it.
- Cloudflare deprecating its WebSocket transport in favor of a multiplexed RPC channel is a signal that for SDK-to-agent traffic a multiplexed RPC connection is preferred over one WebSocket per call.

### Gaps
- Modal's wire transport (believed gRPC, unverified) and Runloop's streaming transport were not found in the pages read.
- Vercel's on-the-wire log format (NDJSON or otherwise) is not documented in the SDK reference.
- Cloudflare's `execStream` SSE behavior was not confirmed for the 1.0 SDK; the docs pages read did not describe it.

## 4. API surface exposed by existing products, and what is table stakes in 2026

### Takeaway
The common core across E2B, Daytona, Modal, Vercel, Cloudflare and Runloop is: create from template/image with timeout and metadata/tags, run command (foreground, background/detached, reconnect by id), file read/write, port preview URLs, snapshot or pause/resume, and Python and TypeScript SDKs. In 2026 persistence (pause/resume or auto-snapshot) and an MCP story have become standard rather than differentiators.

### Cited Findings

E2B (JS/Python SDK 2.5x as of this date)
- Commands: `commands.run()` with `onStdout`/`onStderr` callbacks; `commands.connect(pid)` reattaches. — [E2B commands streaming](https://docs.e2b.dev/docs/commands/streaming); [E2B sandbox lifetime FAQ](https://docs.e2b.dev/faq/sandbox-lifetime)
- Sandbox default timeout is 5 minutes; set `timeoutMs` at create or `setTimeout()` later; `onTimeout` defaults to `'kill'`, `'pause'` is the alternative. — [E2B sandbox lifetime FAQ](https://docs.e2b.dev/faq/sandbox-lifetime)
- Pause/resume: `sandbox.pause()`, `Sandbox.connect()` resumes; default pause saves filesystem and memory including running processes; `keepMemory: false` saves filesystem only and cold-boots on resume. Pause takes about 4 seconds per 1 GiB RAM; resume about 1 second. Paused sandboxes have no TTL. Clients are disconnected while paused and must reconnect. Pause may be refused with HTTP 503 (`ServiceBusyError`, SDK 2.48.0+). `autoResume` defaults to false. — [E2B persistence](https://docs.e2b.dev/sandbox/persistence)
- Code interpreter: `sbx.runCode(code)` / `run_code`; returns `error` (`name`, `value`, `traceback`) and `results` (items may carry `png` base64, charts, tables, text). The code interpreter server is reachable on the sandbox public URL unless `allowPublicTraffic: false`. — [E2B code interpreting](https://docs.e2b.dev/docs/code-interpreting/analyze-data-with-ai)
- Filesystem: `files.write`, `files.watchDir()` (streamed events), pre-signed upload/download URLs generated by the SDK. — [E2B sandbox lifetime FAQ](https://docs.e2b.dev/faq/sandbox-lifetime); [E2B secured access](https://docs.e2b.dev/docs/sandbox/secured-access)
- Lifecycle events API: `GET https://api.e2b.app/events/sandboxes/{sandboxId}` and team-wide `GET /events/sandboxes`; types include `sandbox.lifecycle.created`, `.updated`, `.killed`; 7-day retention. — [E2B lifecycle events API](https://docs.e2b.dev/docs/sandbox/lifecycle-events-api)
- MCP: E2B partnered with Docker to bring MCP support into sandboxes with access to a catalog of MCP servers via Docker's open-source MCP gateway; an official demo repo connects a sandboxed MCP server to a Mastra agent. — [E2B blog](https://www.e2b.dev/blog/docker-e2b-partner-to-introduce-mcp-support-in-e2b-sandbox); [e2b-dev/mcp-gateway-demo](https://github.com/e2b-dev/mcp-gateway-demo)

Daytona
- README features: sandbox creation "in under 90ms", OCI/Docker compatibility, process and code execution, file system operations, Git operations, LSP support, an MCP server, snapshots, webhooks, SSH access, preview, plus SDKs, REST API and CLI. Client libraries for Python, TypeScript, Ruby, Go and Java. — [daytonaio/daytona](https://github.com/daytonaio/daytona)
- Process API: one-off execute (command, cwd, env, timeout), code execution, named sessions that persist state (cwd carries over), stdin to a running session command, session command logs fetched or streamed with stdout/stderr callbacks. — [Daytona Java SDK process docs](https://www.daytona.io/docs/en/java-sdk/process)
- Preview URLs: `https://{port}-{sandboxId}.{daytonaProxyDomain}` for any HTTP port 1 to 65535. — [Daytona preview docs](https://www.daytona.io/docs/en/preview)

Modal
- `modal.Sandbox.create` options include `image`, `volumes`, `secrets`, `timeout`, `idle_timeout`, `readiness_probe`, `runtime` (`gvisor` or `vm`), `name`. Default max lifetime 5 minutes, up to 24 hours via `timeout`; longer runs via filesystem snapshot and restore. `idle_timeout` counts running execs, stdin writes and open tunnel TCP connections as activity. Terminated sandboxes return exit code 137. — [Modal sandbox guide](https://modal.com/docs/guide/sandbox)
- Named sandboxes (`Sandbox.from_name`, unique among running sandboxes in an app, under 64 chars), `set_tags` and `Sandbox.list(tags=...)`, `Sandbox.from_id`. — [Modal sandbox guide](https://modal.com/docs/guide/sandbox)
- `snapshot_filesystem` returns an Image (default timeout 55 seconds, retained 30 days unless `ttl=None`); `tunnels()` returns a dict keyed by container port; `create_connect_token` (alpha) issues credentials for HTTP connections to the sandbox with optional user metadata. — [Modal Sandbox reference](https://modal.com/docs/reference/modal.Sandbox)

Vercel Sandbox (docs last updated 2026-09-22)
- Firecracker microVMs; default image `vercel/sandbox/universal` (Node.js LTS, Python 3.14, coding agents); custom OCI images from Vercel Container Registry; JS SDK `@vercel/sandbox`, Python SDK, CLI. Persistence is the default (auto-save on stop, resume later). — [Vercel Sandbox docs](https://vercel.com/docs/vercel-sandbox)
- `Sandbox.create`: `source` (git, tarball, snapshot), `resources.vcpus` default 2 with 2048 MB per vCPU, up to 15 `ports`, `timeout` default 5 minutes, `networkPolicy`, `persistent` default true, `snapshotExpiration` default 30 days, up to 5 tags. `runCommand({cmd, args, cwd, env, sudo, detached, stdout, stderr, signal})`; `writeFiles`, `readFile` (ReadableStream), `readFileToBuffer`, `mkDir`; `domain(port)`; `extendTimeout(ms)`; `snapshot()` (sandbox shuts down afterward); `stop()` returns `activeCpuDurationMs` and `networkTransfer` for persistent sandboxes. — [Vercel Sandbox SDK reference](https://vercel.com/docs/sandbox/sdk-reference)
- Billing is by active CPU time plus provisioned memory; Vercel cites research that model-inference waiting is 71% to 98% of agent runtime. — [Vercel Sandbox docs](https://vercel.com/docs/vercel-sandbox)

Cloudflare Sandbox SDK (docs last updated 2026-09-30)
- A Durable Object starts a container instance (each "a microVM with its own kernel and network") and runs commands in it; `container.start({image, entrypoint, enableInternet})`, `container.exec(cmd)`, `process.output()`; package adds `Files`, `S3Mount` (bucket mounts), `DirectoryBackup` (save/restore to R2); guides cover streaming output, opening a terminal, background processes and previewing web servers. Requires Workers Paid. A 1.0 line exists with 0.x on a `v0` branch; Apache-2.0. — [Cloudflare Sandbox docs](https://developers.cloudflare.com/sandbox/); [cloudflare/sandbox-sdk](https://github.com/cloudflare/sandbox-sdk)

Runloop
- Devboxes: `devbox.cmd.exec(...)` runs to completion with `await result.stdout()`; `devbox.shutdown()`; snapshot, suspend and resume via API; Blueprints for team-shared custom images (Dockerfile based); starter image has Python 3.12 with pip and uv, Node 22.15.0. Agent Gateways proxy LLM API credentials and MCP Hub connects agents to MCP tool servers without exposing credentials to the devbox. Python (`runloop_api_client`) and TypeScript (`@runloop/api-client`) SDKs, `RUNLOOP_API_KEY`. — [Runloop devbox overview](https://docs.runloop.ai/docs/devboxes/overview)

microsandbox and OpenHands
- microsandbox: SDKs for TypeScript, Rust, Python, Go (Ruby linked); MCP via a separate `microsandbox-mcp` server (`npx -y microsandbox-mcp`); Apache-2.0. — [microsandbox repo](https://github.com/zerocore-ai/microsandbox)
- OpenHands runtime: actions are shell commands, file operations and Python (IPython) code with tracked working directory. — [OpenHands runtime architecture](https://docs.openhands.dev/openhands/usage/architecture/runtime)

### Inferences
- Table stakes for 2026, present in at least four of the six hosted products: create with image/template, timeout and tags/metadata; foreground command with streamed stdout/stderr and exit code; background/detached command with id, reconnect and kill; file read/write (plus bulk upload/download); HTTP port preview URL per port; snapshot and/or pause-resume; list/get/kill sandboxes; Python and TypeScript SDKs; API-key auth.
- Strongly expected but not universal: stateful code interpreter with rich results (E2B, Daytona, OpenHands), interactive PTY (E2B, Daytona, Cloudflare), filesystem watch (E2B), named persistent shell sessions (Daytona), idle timeout distinct from max lifetime (Modal), lifecycle events/webhooks (E2B, Daytona), auto-pause on timeout (E2B, Vercel).
- MCP shows up in three forms: (1) the vendor ships an MCP server exposing sandbox tools (Daytona, microsandbox); (2) MCP servers run inside the sandbox behind a gateway (E2B with Docker); (3) a credential-isolating MCP proxy outside the sandbox (Runloop MCP Hub). A new service should at least ship form (1), since it is a thin wrapper over the REST API, and make form (2) possible through port exposure.
- Naming conventions converge: `Sandbox.create`, `sandbox.commands.run` / `exec` / `runCommand`, `files.read|write`, `getHost(port)` / `domain(port)` / `get_preview_link(port)`, `pause` / `connect`, `setTimeout` / `extendTimeout`, `kill` / `stop`. Following E2B's shape minimizes friction for agent frameworks that already integrate E2B.
- Timeouts need three separate knobs, as the products show: sandbox max lifetime (5-minute defaults at E2B, Modal, Vercel), idle timeout (Modal), and per-command timeout (E2B PTY 60 seconds default).

### Gaps
- Specific agent-framework integrations (LangChain, OpenAI Agents SDK, Claude Agent SDK, Vercel AI SDK, etc.) were not verified against primary sources; only third-party guides appeared in search.
- Daytona and Modal official MCP server details were not confirmed beyond Daytona's README listing "an MCP server".
- E2B template/custom image API, `getHost`, metadata and filesystem method details were not read directly on this pass.
- Cloudflare's higher-level method names (`execStream`, `runCode`, `exposePort`, sessions) could not be confirmed for the current 1.0 SDK from the pages read; they may belong to the 0.x line.
- Runloop details on shells, file ops, tunnels and streaming were not on the page read.

## 5. Authentication, API keys, rate limits, quotas, metering, audit, and per-sandbox tokens / signed URLs

### Takeaway
The established pattern is two credential tiers: a long-lived project API key for the control plane, and a short-scope per-sandbox token (header) or signed URL (token in hostname) for direct data-plane connections to the sandbox, with per-endpoint per-project rate limits returning 429 plus `Retry-After`.

### Cited Findings
- E2B control plane uses a project API key in the `X-API-Key` header. — [E2B lifecycle events API](https://docs.e2b.dev/docs/sandbox/lifecycle-events-api)
- E2B data plane: the sandbox access token (`envdAccessToken`) is returned when a sandbox is created, connected, resumed or fetched, and must be sent as `X-Access-Token` on direct calls to envd. Requires envd `v0.2.0`+; introduced as optional in SDK 1.5.0, default from 2.0.0, and from 2.51.0 the `secure` option is ignored and secure access is always on. — [E2B secured access](https://docs.e2b.dev/docs/sandbox/secured-access)
- E2B's separate account-level access token (`E2B_ACCESS_TOKEN`, CLI login only) is deprecated; requests authenticated with it fail after 2026-08-01. — [E2B access token deprecation](https://e2b.dev/docs/migration/access-token-deprecation) (via search summary)
- E2B file upload/download can use pre-signed URLs generated by the SDK. — [E2B secured access](https://docs.e2b.dev/docs/sandbox/secured-access)
- E2B public port URLs are open by default; `allowPublicTraffic: false` restricts them. — [E2B code interpreting](https://docs.e2b.dev/docs/code-interpreting/analyze-data-with-ai)
- E2B rate limits: API requests 10/sec (Hobby) and 20/sec (Pro) per endpoint per project; sandbox creation 1/sec and 5/sec; concurrent sandboxes 20 (Hobby), 100 to 1,100 (Pro, +500 slots per $500/month add-on); max continuous runtime 1 hour and 24 hours. Exceeding returns HTTP 429 with `Retry-After`; SDKs 2.49.1+ retry automatically. — [E2B rate limits](https://docs.e2b.dev/docs/sandbox/rate-limits)
- Daytona preview auth: standard URL `https://{port}-{sandboxId}.{proxyDomain}` needs the `x-daytona-preview-token` header unless the sandbox is `public`; that token is sandbox-wide, not revocable, rotated on stop/start but kept across pause/resume. Signed URL `https://{port}-{token}.{proxyDomain}` embeds a token bound to a single port, with expiry (default 60 s, min 1, max 86,400) and revocation. The two token types are not interchangeable. — [Daytona preview docs](https://www.daytona.io/docs/en/preview)
- Daytona PTY WebSocket connects on the sandbox's preview endpoint with the preview token passed in a header. — [Daytona Ruby SDK (rubydoc)](https://www.rubydoc.info/gems/daytona/Daytona/Process) (via search summary)
- Modal: `create_connect_token` (alpha) issues credentials for HTTP connections to a sandbox and can carry `user_metadata`; `include_oidc_identity_token=True` injects `MODAL_IDENTITY_TOKEN` into the sandbox. — [Modal Sandbox reference](https://modal.com/docs/reference/modal.Sandbox); [Modal sandbox guide](https://modal.com/docs/guide/sandbox)
- Vercel Sandbox: authentication by Vercel OIDC token (recommended, project-scoped, automatic in production) or access tokens for non-Vercel environments. — [Vercel Sandbox docs](https://vercel.com/docs/vercel-sandbox)
- Runloop: Agent Gateways let agents call LLM APIs, and MCP Hub lets them reach MCP servers, without real credentials entering the devbox. — [Runloop devbox overview](https://docs.runloop.ai/docs/devboxes/overview)
- Metering signals exposed by vendors: Vercel returns `activeCpuDurationMs` and `networkTransfer` on stop and bills active CPU plus provisioned memory. — [Vercel Sandbox SDK reference](https://vercel.com/docs/sandbox/sdk-reference); [Vercel Sandbox docs](https://vercel.com/docs/vercel-sandbox)
- Jupyter signs every message with HMAC-SHA256 using a per-connection key, an example of per-session message authentication inside the sandbox channel. — [Jupyter messaging spec](https://jupyter-client.readthedocs.io/en/stable/messaging.html)

### Inferences
- Gateway design that follows the evidence: API key (hashed at rest, prefix-identifiable, scoped to project/tenant) on the control plane; the create/connect response returns a per-sandbox token that the SDK sends as a header to a data-plane host named `{port}-{sandboxId}.{domain}`; browsers and third parties get signed URLs with a port-bound, expiring, revocable token in the hostname or query. WebSocket from browsers cannot set arbitrary headers, so the signed-URL or `Sec-WebSocket-Protocol`-carried token form is required for browser terminals.
- Make per-sandbox tokens revocable and rotated on restart; Daytona's non-revocable sandbox-wide header token is a documented weakness that its signed-URL variant fixes.
- Rate limiting: token bucket keyed by (tenant, endpoint class), separate buckets for sandbox creation rate and concurrent-sandbox quota, 429 with `Retry-After`, and SDK auto-retry. Enforce concurrency quota transactionally at create time in the control plane, not only at the edge.
- Wildcard preview hostnames mean the edge proxy must route on the Host header to a sandbox and port; this is also the natural enforcement point for per-sandbox tokens, connection caps and byte metering.
- Usage metering dimensions suggested by vendor billing: sandbox wall-clock seconds by resource shape, active CPU time, provisioned memory, network transfer, snapshot/storage bytes. Emit metering as append-only events from the control plane lifecycle (created, paused, resumed, killed) so they double as the audit trail; E2B's lifecycle event schema (`id`, `type`, `sandboxId`, `sandboxTeamId`, `sandboxTemplateId`, `timestamp`, `eventData`) is a usable template.
- Audit logging should record at minimum: API key id, tenant, sandbox id, action, source IP, and for executions the command line (or a hash when tenants opt out), exit code and duration.

### Gaps
- No vendor publishes its gateway implementation of rate limiting, quota enforcement or audit logging; the above design points are inferences.
- E2B's signed URL parameters (signature algorithm, expiration field) and its traffic access token header were not documented on the page read.
- No sourced information on API key format, hashing, or rotation practices at these vendors.
- Vercel's maximum sandbox timeout per plan was not captured.

## 6. Implementation language and libraries for control plane and in-sandbox agent

### Takeaway
Go is the incumbent for in-sandbox agents and the surrounding infrastructure (E2B envd; Daytona's toolbox daemon per its former open repo), with Rust appearing where a tiny static in-guest binary matters (Cloudflare's sandbox-shim, microsandbox). For a Podman-based design Go has the decisive advantage of native Podman bindings and the most-used PTY and WebSocket libraries.

### Cited Findings
- E2B envd is a Go module (`go.mod`, `main.go`) with API specs under `spec/` that generate server stubs (`make generate`); the process service is defined in protobuf and served over Connect RPC. — [e2b-dev/infra packages/envd](https://github.com/e2b-dev/infra/tree/main/packages/envd); [envd process.proto](https://raw.githubusercontent.com/e2b-dev/infra/main/packages/envd/spec/process/process.proto)
- Daytona's public repository is archived and read-only; the README states core development moved to a private codebase as of June 2026 with no further updates. What remains public is `libs/` (SDKs and OpenAPI-generated API and toolbox clients for Python, TypeScript, Ruby, Go, Java); last referenced tag v0.190.0. — [daytonaio/daytona](https://github.com/daytonaio/daytona)
- Cloudflare sandbox-sdk: TypeScript package plus an in-container Linux helper `sandbox-shim`; repo contains `Cargo.toml` and `tsconfig.json`; Apache-2.0; 990 commits. — [cloudflare/sandbox-sdk](https://github.com/cloudflare/sandbox-sdk)
- microsandbox: Rust (`cargo add microsandbox`), built on libkrun and smoltcp, SDK embeds the runtime; Apache-2.0. — [microsandbox repo](https://github.com/zerocore-ai/microsandbox)
- OpenHands runtime server is Python (`pyproject.toml`, `poetry.lock`, IPython plugin). — [OpenHands runtime architecture](https://docs.openhands.dev/openhands/usage/architecture/runtime)
- creack/pty latest is v1.1.24 (published 2024-10-31). `StartWithSize` assigns the tty to the command's stdin/stdout/stderr, starts it in a new session with a controlling terminal, and sizes the pty first; `Setsize(*os.File, *Winsize{Rows, Cols, X, Y})` resizes; `StartWithAttrs` since v1.1.10. `kr/pty` is deprecated in its favor. — [pkg.go.dev creack/pty](https://pkg.go.dev/github.com/creack/pty)
- coder/websocket (formerly nhooyr.io/websocket; maintained by Coder since 2024): zero dependencies, first-class `context.Context`, concurrent writes supported, `net.Conn` wrapper, full RFC 7692 permessage-deflate, `wsjson` helpers, compiles to Wasm, passes Autobahn; HTTP/2 WebSocket not yet supported (roadmap issue #4). Its README says gorilla/websocket is mature and widely used with prepared writes and configurable buffers but requires a pong-handler registration and supports only no-context-takeover compression; `golang.org/x/net/websocket` is deprecated. These comparisons are the project's own claims. — [coder/websocket](https://github.com/coder/websocket)
- Rust portable-pty: most recent version confirmed is 0.9.0 (Debian tracker entry dated 2026-07-29); extracted from WezTerm; size passed as `PtySize { rows, cols, .. }` to `openpty`. A fork `xpty` (0.3.1) aims to modernize it. — [Debian tracker rust-portable-pty](https://tracker.debian.org/pkg/rust-portable-pty); [xpty on docs.rs](https://docs.rs/crate/xpty/0.3.1)
- Modal's public SDKs are Python, JavaScript and Go. — [Modal Sandbox reference](https://modal.com/docs/reference/modal.Sandbox)

### Inferences
- Recommended stack for a rootless-Podman service: Go for both gateway/control plane and in-sandbox agent. Rationale: Podman's own Go bindings (`github.com/containers/podman/v5/pkg/bindings`) talk to the same libpod REST endpoints cited in section 1; creack/pty is the de facto PTY library; a single language lets agent and gateway share protobuf-generated types; a statically linked Go binary can be bind-mounted into arbitrary user images.
- Agent protocol: protobuf schema served with Connect (as envd does) gives unary plus server-streaming over plain HTTP/1.1 and HTTP/2, JSON or binary encoding, and generated TypeScript and Python clients. Add a WebSocket endpoint only for the interactive PTY path.
- WebSocket library: coder/websocket for context-aware cancellation and safe concurrent writes (useful when output, heartbeat and exit frames are written from different goroutines); gorilla/websocket if prepared messages or tuned buffer sizes matter. Either works; neither does backpressure for you beyond blocking writes, so the PTY read loop must be coupled to write completion.
- Rust (tokio, axum, portable-pty) is the better choice only if agent memory footprint and startup time inside very small sandboxes dominate, or if the team is already Rust; Cloudflare and microsandbox show it is viable. It forfeits the Podman Go bindings (the REST API would be called directly over the Unix socket).
- Daytona going closed-source in June 2026 removes it as a reference implementation going forward; E2B's `e2b-dev/infra` is the main open reference for a Go in-sandbox agent.

### Gaps
- containers/podman Go bindings version and API stability were not checked on this pass; the bindings recommendation is from background knowledge (unverified).
- envd's actual dependency list (connectrpc, chi, creack/pty) and its listening port (commonly cited as 49983) were not confirmed from `go.mod` or source.
- Daytona daemon/runner implementation languages can no longer be read from the public repo; prior knowledge that the daemon was Go and the API NestJS is unverified here.
- gorilla/websocket's current release and maintenance status were not checked directly.
- The latest portable-pty version on crates.io was not confirmed beyond 0.9.0.

## 7. Observability and output persistence

### Takeaway
Public documentation on sandbox observability is thin: vendors expose lifecycle event feeds, per-command retrievable output, and usage counters, but none of the pages read describes an OpenTelemetry pipeline, so the tracing/metrics design below is mostly inference.

### Cited Findings
- E2B exposes a lifecycle events feed per sandbox and per team with event fields `version`, `id`, `type`, `eventData`, `sandboxBuildId`, `sandboxExecutionId`, `sandboxId`, `sandboxTeamId`, `sandboxTemplateId`, `timestamp`; pagination via `offset`, `limit` (default 10, max 100), `orderAsc`, `types`; retention 7 days by default. — [E2B lifecycle events API](https://docs.e2b.dev/docs/sandbox/lifecycle-events-api)
- E2B lists `/sandboxes/metrics` among its rate-limited GET endpoints, indicating a sandbox metrics API. — [E2B rate limits](https://docs.e2b.dev/docs/sandbox/rate-limits)
- Vercel persists command output so it can be fetched after the fact: `sandbox.getCommand(cmdId)` then `cmd.output('both')`, and `stdout()` / `stderr()` return full output as strings; `stop()` reports `activeCpuDurationMs` and `networkTransfer`. — [Vercel Sandbox SDK reference](https://vercel.com/docs/sandbox/sdk-reference)
- Daytona session command logs can be fetched after the fact or streamed asynchronously with stdout and stderr callbacks. — [Daytona Java SDK process docs](https://www.daytona.io/docs/en/java-sdk/process)
- Podman attach with `logs=true` replays the container's log before the live stream. — [podman register_containers.go](https://raw.githubusercontent.com/containers/podman/main/pkg/api/server/register_containers.go)
- Podman exec sessions (and their inspect data, including exit code) are removed 5 minutes after exit, so exit status must be captured and persisted by the gateway or agent. — [podman register_exec.go](https://raw.githubusercontent.com/containers/podman/main/pkg/api/server/register_exec.go)
- Jupyter's `parent_header` links every output message to the request that caused it, and `status` busy/idle brackets each execution; this is a ready-made correlation model for per-execution logs. — [Jupyter messaging spec](https://jupyter-client.readthedocs.io/en/stable/messaging.html)
- Modal distinguishes lifecycle states Created, Scheduled, Started, Ready (with readiness probe), Finished; readiness probes run for at most 5 minutes with a 100 ms default interval. — [Modal sandbox guide](https://modal.com/docs/guide/sandbox)

### Inferences
- Give every execution a server-generated execution id and persist a record: tenant, sandbox id, execution id, command or code hash, mode (pipe/PTY/interpreter), start and end timestamps, exit code, signal, termination reason, bytes of stdout/stderr, truncation flag. This is what makes Vercel-style `getCommand(id).output()` and Daytona-style "get session command logs" possible.
- Output persistence in two tiers: an in-agent bounded ring buffer per process for live reconnect and replay, and asynchronous shipping of the full (capped) stream to object storage or a log store keyed by execution id for after-the-fact retrieval and audit. Store stdout and stderr as separate interleaved records with offsets and timestamps so both merged and split views can be reconstructed.
- Tracing: propagate W3C trace context from the SDK request through gateway, scheduler, and agent, with spans for sandbox create (queue, image pull, start, agent ready), exec (dispatch, first byte, exit) and file operations. Accept a caller-supplied trace parent so agent frameworks can stitch sandbox spans into their own traces. This is standard OpenTelemetry practice but is not documented by any vendor page read.
- Metrics worth having at the gateway: active WebSocket/stream connections, time to first output byte, stream disconnects by cause (client, edge timeout, backend drain), reconnect count, bytes in/out per tenant, dropped/truncated output bytes, backpressure pause duration, exec duration and exit-code histograms, 429 counts per tenant and endpoint, plus per-sandbox CPU/memory/disk samples as E2B's metrics endpoint implies.
- Lifecycle events should be a durable append-only stream that feeds billing, webhooks and the audit log from one source, mirroring E2B's events API shape.

### Gaps
- No primary source was found describing OpenTelemetry usage, log pipelines or output storage backends inside E2B, Daytona, Modal, Vercel, Cloudflare or Runloop.
- E2B's metrics API fields and sampling interval were not read.
- No sourced numbers for output retention periods or per-execution log size limits at any vendor (only E2B's 7-day lifecycle event retention).
- Webhook support was only confirmed as a README bullet for Daytona; payload formats were not researched.
