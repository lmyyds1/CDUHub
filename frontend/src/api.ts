export type SourceKind = 'department' | 'college'
export interface Channel { id: string; name: string; url: string }
export interface Source {
  id: string; name: string; url: string; kind: SourceKind; group: string;
  channels: Channel[]; status: 'pending' | 'success' | 'ok' | 'partial' | 'error' | 'unavailable' | 'empty';
  lastCrawled: string; error: string; articleCount: number;
}
export interface Article {
  id: string; title: string; content?: string; url: string; sourceId: string; sourceName: string;
  sourceKind: SourceKind; channel: string; date: string; dateText: string; fetchedAt: string;
}
export interface SyncStatus {
  running: boolean; phase: string; current: string; completed: number; total: number;
  startedAt: string; finishedAt: string; errors: string[];
}
export interface Snapshot {
  sources: Source[]; articles: Article[]; sync: SyncStatus; lastUpdated: string;
}
export interface SyncOptions { sourceId: string; pagesPerChannel: number; maxChannelsPerSource: number }

declare global {
  interface Window {
    go?: { main?: { App?: {
      GetSnapshot(): Promise<Snapshot>;
      StartSync(options: SyncOptions): Promise<{ ok: boolean } | void>;
      CancelSync(): Promise<unknown>;
      OpenURL(url: string): Promise<unknown>;
    } } };
  }
}

export const emptySnapshot: Snapshot = {
  sources: [], articles: [], lastUpdated: '',
  sync: { running: false, phase: '', current: '', completed: 0, total: 0, startedAt: '', finishedAt: '', errors: [] },
}

const REQUEST_TIMEOUT = 8000
const TIMEOUT_MESSAGE = '服务响应超时，请稍后重试'
const NETWORK_MESSAGE = '暂时无法连接信息服务，请稍后重试'
const FORMAT_MESSAGE = '服务返回的数据格式异常，请稍后重试'

// 优先使用 AbortSignal.timeout（现代 WebView / 浏览器），旧环境回退到 AbortController + setTimeout。
function timeoutSignal(): { signal?: AbortSignal; cancel: () => void } {
  if (typeof AbortSignal !== 'undefined' && typeof AbortSignal.timeout === 'function') return { signal: AbortSignal.timeout(REQUEST_TIMEOUT), cancel: () => {} }
  if (typeof AbortController === 'undefined') return { cancel: () => {} }
  const controller = new AbortController()
  const timer = setTimeout(() => controller.abort(), REQUEST_TIMEOUT)
  return { signal: controller.signal, cancel: () => clearTimeout(timer) }
}

// Wails bridge 调用没有 fetch 的 signal 可用，用 Promise 竞速兜底，避免 bridge 挂起导致 loading 永真。
function withTimeout<T>(task: Promise<T>): Promise<T> {
  return new Promise<T>((resolve, reject) => {
    const timer = setTimeout(() => reject(new Error(TIMEOUT_MESSAGE)), REQUEST_TIMEOUT)
    task.then(
      value => { clearTimeout(timer); resolve(value) },
      error => { clearTimeout(timer); reject(error) },
    )
  })
}

function isAbortError(error: unknown) {
  const name = typeof error === 'object' && error !== null ? (error as { name?: string }).name || '' : ''
  return name === 'AbortError' || name === 'TimeoutError'
}

// 先取文本再尝试解析，避免用 throw 做控制流；error 字段不是字符串时不会出现 [object Object]。
async function responseError(response: Response) {
  let raw = ''
  try { raw = await response.text() } catch { raw = '' }
  const body = raw.trim()
  let detail = ''
  if (body) {
    let parsed: unknown
    let isJSON = true
    try { parsed = JSON.parse(body) } catch { isJSON = false }
    if (isJSON) {
      const field = typeof parsed === 'object' && parsed !== null ? (parsed as { error?: unknown }).error : undefined
      if (typeof field === 'string' && field.trim()) detail = field.trim()
    } else if (!body.includes('<') && body.length <= 200) {
      // 纯文本错误体（如反向代理返回的说明）直接采用，HTML 错误页则回落到状态码文案。
      detail = body
    }
  }
  return new Error(detail || `服务请求失败 (${response.status})`)
}

async function request<T>(path: string, body?: unknown): Promise<T> {
  const timeout = timeoutSignal()
  const init: RequestInit = { signal: timeout.signal }
  if (body !== undefined) {
    init.method = 'POST'
    init.headers = { 'Content-Type': 'application/json' }
    init.body = JSON.stringify(body)
  }
  try {
    let response: Response
    try { response = await fetch(path, init) }
    catch (error) { throw new Error(isAbortError(error) ? TIMEOUT_MESSAGE : NETWORK_MESSAGE) }
    if (!response.ok) throw await responseError(response)
    try { return await response.json() as T }
    catch { throw new Error(FORMAT_MESSAGE) }
  } finally { timeout.cancel() }
}

export async function getSnapshot(): Promise<Snapshot> {
  const bridge = window.go?.main?.App
  const data = bridge ? await withTimeout(bridge.GetSnapshot()) : await request<Snapshot>('/api/snapshot')
  const safe = data || emptySnapshot
  return {
    ...emptySnapshot, ...safe,
    sources: (safe.sources || []).map(source => ({ ...source, channels: source.channels || [] })),
    articles: safe.articles || [],
    sync: { ...emptySnapshot.sync, ...safe.sync, errors: safe.sync?.errors || [] },
  }
}

export async function startSync(options: SyncOptions) {
  const bridge = window.go?.main?.App
  const result = bridge ? await withTimeout(bridge.StartSync(options)) : await request<{ ok: boolean }>('/api/sync', options)
  if (result && !result.ok) throw new Error('未能启动同步，请稍后重试。')
}
export async function cancelSync() {
  const bridge = window.go?.main?.App
  return bridge ? withTimeout(bridge.CancelSync()) : request('/api/cancel', {})
}
export function isSafeURL(url: string) {
  try { return ['https:', 'http:'].includes(new URL(url).protocol) } catch { return false }
}
export async function openURL(url: string) {
  if (!isSafeURL(url)) throw new Error('这个来源暂未提供有效的网页地址。')
  const bridge = window.go?.main?.App
  if (bridge) await bridge.OpenURL(url)
  else window.open(url, '_blank', 'noopener,noreferrer')
}
