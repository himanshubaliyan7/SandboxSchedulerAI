/**
 * Public data types and the conversion from the server's snake_case JSON.
 * Converters tolerate unknown fields and unknown enum values: a newer
 * server must not break an older SDK.
 */

import { parseDate } from './util.js'

/** A string union that still accepts values this SDK version does not know. */
type Open<T extends string> = T | (string & {})

export type SandboxState = Open<'assigned' | 'running' | 'recycling' | 'destroyed' | 'failed' | 'unknown'>

export type TerminationReason = Open<
  'released' | 'idle_timeout' | 'max_lifetime' | 'oom_killed' | 'instance_lost' | 'deadline_exceeded' | 'host_drain' | 'error'
>

export type NetworkMode = Open<'none' | 'allowlist' | 'full'>

/** Egress policy. Deny always wins; private, loopback and metadata ranges are always denied by the server. */
export interface NetworkPolicy {
  /** `none`, `allowlist` or `full`. Omitted means the template's mode. */
  mode?: NetworkMode
  /** Exact hostnames, or `*.example.com` (any depth of subdomain, not the apex). */
  allowDomains?: string[]
  /** Permits IP-literal targets only. */
  allowCidrs?: string[]
  denyCidrs?: string[]
}

export interface SandboxInfo {
  sandboxId: string
  templateId: string
  templateVersion: number
  state: SandboxState
  /** Why the sandbox ended; absent while it lives. */
  reason?: TerminationReason
  /** When the container was created (before it was claimed). */
  createdAt?: Date
  /** When it was assigned to the tenant. */
  startedAt?: Date
  lastActivityAt?: Date
  /** When the maximum lifetime ends. Absent if the template has none. */
  expiresAt?: Date
  network?: NetworkPolicy
  metadata: Record<string, string>
}

export interface SandboxEvent {
  id: number
  /** `claimed`, `transition`, `command_started`, `command_finished`, ... */
  type: string
  fromState?: SandboxState
  toState?: SandboxState
  reason?: TerminationReason
  at?: Date
  /** Type-specific payload, as the server sent it (snake_case keys). */
  data: Record<string, unknown>
}

/** Resource counters read from the sandbox's cgroup. */
export interface SandboxStats {
  memoryBytes: number
  oomKills: number
  cpuUsageUsec: number
  cpuThrottledPeriods: number
  cpuThrottledUsec: number
  pidsCurrent: number
  /** Forks refused by the pids limit (a fork bomb shows here). */
  pidsLimitHits: number
  memoryHighHits: number
}

/** What `files.write` reports about the file it wrote (E2B: `WriteInfo`, plus `size`). */
export interface WriteInfo {
  /** The file's base name. */
  name: string
  type: 'file'
  /** The absolute path; a relative path was resolved against `/workspace`. */
  path: string
  /** Bytes written. */
  size: number
}

export interface TemplateInfo {
  templateId: string
  version: number
  image: string
  /** The isolation tier: `crun` (a hardened container on the host kernel) or `runsc` (gVisor). */
  runtime: string
  /**
   * Whether sandboxes of this template run directly on the host kernel.
   * `undefined` when the server cannot say (the template names a runtime
   * the server has not enabled).
   */
  sharedKernel: boolean | undefined
  resources: {
    cpus: number
    memoryBytes: number
    pidsLimit: number
    workdirBytes: number
    tmpBytes: number
    nofileLimit: number
  }
  /** The widest network mode a sandbox of this template may ask for. */
  network: NetworkMode
  /**
   * The template's default allowlist: the hosts (and, by address, the
   * CIDRs) a sandbox created without a `network` option of its own may
   * reach when `network` is `allowlist`. A `network` option replaces
   * these; it does not add to them.
   */
  allowDomains: string[]
  allowCidrs: string[]
  /** 0 means none. */
  idleTimeoutMs: number
  /** 0 means none. */
  maxLifetimeMs: number
}

export type ExitReason = Open<'exited' | 'killed' | 'timeout' | 'oom'>

export interface ExitStatus {
  /** 128 + the signal number if the command was ended by a signal (137 for SIGKILL). */
  exitCode: number
  reason: ExitReason
  signal?: string
}

export interface CommandInfo {
  /** Opaque command id issued by the in-sandbox agent. It is not a kernel pid. */
  pid: string
  tag?: string
  cmd: string
  args: string[]
  pty: boolean
  startedAt?: Date
  /** Present once the command has exited. */
  exit?: ExitStatus
  /** Total output bytes so far: the offset the next output byte will get. */
  nextOffset: number
}

export interface EntryInfo {
  name: string
  /** Absolute path inside the sandbox. */
  path: string
  type: 'file' | 'dir'
  size: number
  /** Octal permission bits, e.g. `0644`. */
  mode: string
  modifiedTime?: Date
}

export type Signal = Open<'SIGINT' | 'SIGTERM' | 'SIGKILL' | 'SIGHUP' | 'SIGQUIT'>

// --- wire conversion ------------------------------------------------------

type Wire = Record<string, unknown>

function obj(v: unknown): Wire {
  return typeof v === 'object' && v !== null && !Array.isArray(v) ? (v as Wire) : {}
}

function str(v: unknown, fallback = ''): string {
  return typeof v === 'string' ? v : fallback
}

function num(v: unknown, fallback = 0): number {
  return typeof v === 'number' && Number.isFinite(v) ? v : fallback
}

function strings(v: unknown): string[] {
  return Array.isArray(v) ? v.filter((x): x is string => typeof x === 'string') : []
}

function stringMap(v: unknown): Record<string, string> {
  const out: Record<string, string> = {}
  for (const [k, val] of Object.entries(obj(v))) if (typeof val === 'string') out[k] = val
  return out
}

function setIf<T, K extends keyof T>(target: T, key: K, value: T[K] | undefined): void {
  if (value !== undefined) target[key] = value
}

export function networkPolicyFromWire(v: unknown): NetworkPolicy | undefined {
  if (typeof v !== 'object' || v === null) return undefined
  const w = obj(v)
  const out: NetworkPolicy = {}
  if (typeof w.mode === 'string' && w.mode !== '') out.mode = w.mode
  if (Array.isArray(w.allow_domains)) out.allowDomains = strings(w.allow_domains)
  if (Array.isArray(w.allow_cidrs)) out.allowCidrs = strings(w.allow_cidrs)
  if (Array.isArray(w.deny_cidrs)) out.denyCidrs = strings(w.deny_cidrs)
  return out
}

export function networkPolicyToWire(p: NetworkPolicy): Wire {
  const out: Wire = {}
  if (p.mode !== undefined) out.mode = p.mode
  if (p.allowDomains !== undefined) out.allow_domains = p.allowDomains
  if (p.allowCidrs !== undefined) out.allow_cidrs = p.allowCidrs
  if (p.denyCidrs !== undefined) out.deny_cidrs = p.denyCidrs
  return out
}

export function sandboxInfoFromWire(v: unknown): SandboxInfo {
  const w = obj(v)
  const info: SandboxInfo = {
    sandboxId: str(w.id),
    templateId: str(w.template_id),
    templateVersion: num(w.template_version),
    state: str(w.state, 'unknown'),
    metadata: stringMap(w.metadata),
  }
  if (typeof w.reason === 'string' && w.reason !== '') info.reason = w.reason
  setIf(info, 'createdAt', parseDate(w.created_at))
  setIf(info, 'startedAt', parseDate(w.started_at))
  setIf(info, 'lastActivityAt', parseDate(w.last_activity_at))
  setIf(info, 'expiresAt', parseDate(w.expires_at))
  setIf(info, 'network', networkPolicyFromWire(w.network))
  return info
}

export function sandboxEventFromWire(v: unknown): SandboxEvent {
  const w = obj(v)
  const ev: SandboxEvent = { id: num(w.id), type: str(w.type), data: obj(w.data) }
  if (typeof w.from_state === 'string' && w.from_state !== '') ev.fromState = w.from_state
  if (typeof w.to_state === 'string' && w.to_state !== '') ev.toState = w.to_state
  if (typeof w.reason === 'string' && w.reason !== '') ev.reason = w.reason
  setIf(ev, 'at', parseDate(w.at))
  return ev
}

export function statsFromWire(v: unknown): SandboxStats {
  const w = obj(v)
  return {
    memoryBytes: num(w.memory_bytes),
    oomKills: num(w.oom_kills),
    cpuUsageUsec: num(w.cpu_usage_usec),
    cpuThrottledPeriods: num(w.cpu_throttled_periods),
    cpuThrottledUsec: num(w.cpu_throttled_usec),
    pidsCurrent: num(w.pids_current),
    pidsLimitHits: num(w.pids_limit_hits),
    memoryHighHits: num(w.memory_high_hits),
  }
}

export function templateFromWire(v: unknown): TemplateInfo {
  const w = obj(v)
  const r = obj(w.resources)
  return {
    templateId: str(w.id),
    version: num(w.version),
    image: str(w.image),
    runtime: str(w.runtime),
    sharedKernel: typeof w.shared_kernel === 'boolean' ? w.shared_kernel : undefined,
    resources: {
      cpus: num(r.cpus),
      memoryBytes: num(r.memory_bytes),
      pidsLimit: num(r.pids_limit),
      workdirBytes: num(r.workdir_bytes),
      tmpBytes: num(r.tmp_bytes),
      nofileLimit: num(r.nofile_limit),
    },
    network: str(w.network, 'none'),
    allowDomains: strings(w.allow_domains),
    allowCidrs: strings(w.allow_cidrs),
    idleTimeoutMs: num(w.idle_timeout_ms),
    maxLifetimeMs: num(w.max_lifetime_ms),
  }
}

export function exitFromWire(v: unknown): ExitStatus | undefined {
  if (typeof v !== 'object' || v === null) return undefined
  const w = obj(v)
  const exit: ExitStatus = { exitCode: num(w.exit_code, -1), reason: str(w.reason, 'exited') }
  if (typeof w.signal === 'string' && w.signal !== '') exit.signal = w.signal
  return exit
}

export function commandInfoFromWire(v: unknown): CommandInfo {
  const w = obj(v)
  const info: CommandInfo = {
    pid: str(w.pid),
    cmd: str(w.cmd),
    args: strings(w.args),
    pty: w.pty === true,
    nextOffset: num(w.next_offset),
  }
  if (typeof w.tag === 'string' && w.tag !== '') info.tag = w.tag
  setIf(info, 'startedAt', parseDate(w.started_at))
  setIf(info, 'exit', exitFromWire(w.exit))
  return info
}

export function entryFromWire(v: unknown): EntryInfo {
  const w = obj(v)
  const entry: EntryInfo = {
    name: str(w.name),
    path: str(w.path),
    type: w.is_dir === true ? 'dir' : 'file',
    size: num(w.size),
    mode: str(w.mode),
  }
  setIf(entry, 'modifiedTime', parseDate(w.mod_time))
  return entry
}

export function list(v: unknown, key: string): unknown[] {
  const items = obj(v)[key]
  return Array.isArray(items) ? items : []
}
