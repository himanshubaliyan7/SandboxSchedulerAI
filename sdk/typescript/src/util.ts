/** Small runtime-neutral helpers. Nothing here may import a Node module. */

interface MaybeProcess {
  env?: Record<string, string | undefined>
  versions?: { node?: string }
}

/** `process`, when the runtime has one (Node, Bun, Deno's compat layer). */
export function runtimeProcess(): MaybeProcess | undefined {
  const p = (globalThis as { process?: MaybeProcess }).process
  return typeof p === 'object' && p !== null ? p : undefined
}

/** Reads an environment variable where a `process.env` exists; undefined elsewhere. */
export function env(name: string): string | undefined {
  try {
    const v = runtimeProcess()?.env?.[name]
    return v === undefined || v === '' ? undefined : v
  } catch {
    return undefined
  }
}

/** True on a browser main thread or worker, where an API key must never be used. */
export function isBrowser(): boolean {
  const g = globalThis as { window?: { document?: unknown }; importScripts?: unknown; WorkerGlobalScope?: unknown }
  if (runtimeProcess()?.versions?.node !== undefined) return false
  if (typeof g.window === 'object' && g.window !== null && typeof g.window.document === 'object') return true
  return typeof g.importScripts === 'function' && typeof g.WorkerGlobalScope !== 'undefined'
}

const encoder = new TextEncoder()

export function utf8(s: string): Uint8Array {
  return encoder.encode(s)
}

type Base64Ctor = { fromBase64?: (s: string) => Uint8Array }

/** Decodes standard padded base64 into bytes. */
export function decodeBase64(b64: string): Uint8Array {
  if (b64 === '') return new Uint8Array(0)
  const native = (Uint8Array as unknown as Base64Ctor).fromBase64
  if (typeof native === 'function') return native.call(Uint8Array, b64)
  const bin = atob(b64)
  const out = new Uint8Array(bin.length)
  for (let i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i)
  return out
}

export function concatBytes(chunks: readonly Uint8Array[], total?: number): Uint8Array {
  const size = total ?? chunks.reduce((n, c) => n + c.byteLength, 0)
  const out = new Uint8Array(size)
  let at = 0
  for (const c of chunks) {
    out.set(c, at)
    at += c.byteLength
  }
  return out
}

/** The reason of an aborted signal as something throwable. */
export function abortReason(signal: AbortSignal): unknown {
  if (signal.reason !== undefined) return signal.reason
  return new DOMException('This operation was aborted', 'AbortError')
}

export function throwIfAborted(signal: AbortSignal | undefined): void {
  if (signal?.aborted) throw abortReason(signal)
}

/** Resolves after `ms`, or rejects with the signal's reason as soon as it aborts. */
export function sleep(ms: number, signal?: AbortSignal): Promise<void> {
  return new Promise<void>((resolve, reject) => {
    if (signal?.aborted) {
      reject(abortReason(signal))
      return
    }
    const onAbort = (): void => {
      clearTimeout(timer)
      reject(abortReason(signal as AbortSignal))
    }
    const timer = setTimeout(() => {
      signal?.removeEventListener('abort', onAbort)
      resolve()
    }, ms)
    signal?.addEventListener('abort', onAbort, { once: true })
  })
}

/**
 * An AbortController that also aborts when any of `signals` does. `dispose`
 * removes the listeners; call it when the operation is over.
 */
export function linkedAbort(...signals: (AbortSignal | undefined)[]): { controller: AbortController; dispose: () => void } {
  const controller = new AbortController()
  const cleanups: (() => void)[] = []
  for (const s of signals) {
    if (!s) continue
    if (s.aborted) {
      controller.abort(abortReason(s))
      break
    }
    const onAbort = (): void => controller.abort(abortReason(s))
    s.addEventListener('abort', onAbort, { once: true })
    cleanups.push(() => s.removeEventListener('abort', onAbort))
  }
  return {
    controller,
    dispose: () => {
      for (const c of cleanups) c()
    },
  }
}

export interface Deferred<T> {
  promise: Promise<T>
  resolve: (value: T) => void
  reject: (reason: unknown) => void
  settled: boolean
}

export function deferred<T>(): Deferred<T> {
  let resolve!: (value: T) => void
  let reject!: (reason: unknown) => void
  const d = {
    settled: false,
    promise: new Promise<T>((res, rej) => {
      resolve = res
      reject = rej
    }),
  } as Deferred<T>
  d.resolve = (v) => {
    d.settled = true
    resolve(v)
  }
  d.reject = (e) => {
    d.settled = true
    reject(e)
  }
  // A rejection nobody is waiting for must not crash the process; whoever
  // awaits the promise later still sees it.
  d.promise.catch(() => {})
  return d
}

/** Exponential backoff with full jitter on the upper half: attempt 0 is about `initialMs`. */
export function backoffDelay(attempt: number, initialMs: number, maxMs: number): number {
  const ceiling = Math.min(maxMs, initialMs * 2 ** Math.min(attempt, 30))
  return Math.round(ceiling / 2 + Math.random() * (ceiling / 2))
}

export function parseDate(v: unknown): Date | undefined {
  if (typeof v !== 'string' || v === '') return undefined
  const d = new Date(v)
  return Number.isNaN(d.getTime()) ? undefined : d
}
