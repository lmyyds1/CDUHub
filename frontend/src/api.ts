export type SourceKind = 'department' | 'college'
export interface Channel { id: string; name: string; url: string }
export interface Source {
  id: string; name: string; url: string; kind: SourceKind; group: string;
  channels: Channel[]; status: 'pending' | 'success' | 'ok' | 'partial' | 'error' | 'unavailable' | 'empty';
  lastCrawled: string; error: string; articleCount: number;
}
export interface Article {
  id: string; title: string; content?: string; url: string; sourceId: string; sourceName: string;
  sourceKind: SourceKind; channel: string; date: string; dateText: string;
  dateInferred: boolean; fetchedAt: string;
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

async function request<T>(path: string, body?: unknown): Promise<T> {
  const response = await fetch(path, body === undefined ? undefined : {
    method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body),
  })
  if (!response.ok) {
    const message = await response.text()
    try { throw new Error(JSON.parse(message).error || `请求失败 (${response.status})`) }
    catch (error) {
      if (error instanceof SyntaxError) throw new Error(`服务请求失败 (${response.status})`)
      throw error
    }
  }
  return response.json()
}

export async function getSnapshot(): Promise<Snapshot> {
  const bridge = window.go?.main?.App
  const data = bridge ? await bridge.GetSnapshot() : await request<Snapshot>('/api/snapshot')
  return {
    ...emptySnapshot, ...data,
    sources: (data.sources || []).map(source => ({ ...source, channels: source.channels || [] })),
    articles: data.articles || [],
    sync: { ...emptySnapshot.sync, ...data.sync, errors: data.sync?.errors || [] },
  }
}

export async function startSync(options: SyncOptions) {
  const bridge = window.go?.main?.App
  const result = bridge ? await bridge.StartSync(options) : await request<{ ok: boolean }>('/api/sync', options)
  if (result && !result.ok) throw new Error('未能启动同步，请稍后重试。')
}
export async function cancelSync() {
  const bridge = window.go?.main?.App
  return bridge ? bridge.CancelSync() : request('/api/cancel', {})
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
