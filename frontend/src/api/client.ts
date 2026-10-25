// Tek API istemcisi: Bearer token, 401'de refresh token ile bir kez yenileme,
// standart hata zarfı {"error":{"code","message","details"}} → ApiError.
export class ApiError extends Error {
  constructor(public status: number, public code: string, message: string, public details?: Record<string, unknown>) {
    super(message)
  }
}

type Tokens = { access_token: string; refresh_token: string }
const KEY = 'enerjihat.tokens'

function loadTokens(): Tokens | null {
  try {
    const raw = localStorage.getItem(KEY)
    return raw ? (JSON.parse(raw) as Tokens) : null
  } catch {
    return null
  }
}
let tokens: Tokens | null = loadTokens()
let onLogout: () => void = () => {}

export function setTokens(t: Tokens | null) {
  tokens = t
  try {
    if (t) localStorage.setItem(KEY, JSON.stringify(t))
    else localStorage.removeItem(KEY)
  } catch {
    /* depolama kapalı olabilir */
  }
}
export const hasTokens = () => tokens !== null
export const onSessionEnd = (fn: () => void) => (onLogout = fn)

export function idemKey(): string {
  return (crypto.randomUUID?.() ?? `${Date.now()}-${Math.random()}`).replace(/[^a-zA-Z0-9-]/g, '')
}

let refreshing: Promise<boolean> | null = null
async function refresh(): Promise<boolean> {
  if (!tokens) return false
  refreshing ??= (async () => {
    const res = await fetch('/api/v1/auth/refresh', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ refresh_token: tokens!.refresh_token }),
    })
    if (!res.ok) return false
    setTokens(await res.json())
    return true
  })().finally(() => (refreshing = null))
  return refreshing
}

export async function api<T = unknown>(
  path: string,
  opts: { method?: string; body?: unknown; idempotent?: boolean | string; retry?: boolean } = {},
): Promise<T> {
  const headers: Record<string, string> = { 'Content-Type': 'application/json' }
  if (tokens) headers.Authorization = `Bearer ${tokens.access_token}`
  if (opts.idempotent) headers['Idempotency-Key'] = typeof opts.idempotent === 'string' ? opts.idempotent : idemKey()
  let res: Response
  try {
    res = await fetch(`/api/v1${path}`, {
      method: opts.method ?? (opts.body !== undefined ? 'POST' : 'GET'),
      headers,
      body: opts.body !== undefined ? JSON.stringify(opts.body) : undefined,
    })
  } catch {
    throw new ApiError(0, 'network', 'Sunucuya ulaşılamıyor. Bağlantınızı kontrol edin.')
  }
  if (res.status === 401 && tokens && opts.retry !== false && !path.startsWith('/auth/')) {
    if (await refresh()) return api<T>(path, { ...opts, retry: false })
    setTokens(null)
    onLogout()
  }
  if (res.status === 204) return undefined as T
  const text = await res.text()
  const data = text ? JSON.parse(text) : undefined
  if (!res.ok) {
    const e = data?.error ?? {}
    throw new ApiError(res.status, e.code ?? 'error', e.message ?? `HTTP ${res.status}`, e.details)
  }
  return data as T
}
