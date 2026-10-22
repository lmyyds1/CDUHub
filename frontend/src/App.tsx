import { useCallback, useDeferredValue, useEffect, useMemo, useRef, useState } from 'react'
import type { MouseEvent, ReactNode } from 'react'
import Icon from './Icon'
import type { IconName } from './Icon'
import { cancelSync, emptySnapshot, getSnapshot, isSafeURL, openURL, startSync } from './api'
import type { Article, Snapshot, Source, SourceKind, SyncOptions } from './api'

type View = 'feed' | 'sources' | 'saved'
type Kind = SourceKind | 'all'
type SearchMode = 'contains' | 'fuzzy' | 'smart'
type Modal = 'sync' | 'about' | null
const PAGE_SIZE = 15
const number = new Intl.NumberFormat('zh-CN')
// 共享 collator：避免每次比较都新建 Intl.Collator（全量 1.8 万条排序的主要开销）。
const zhCollator = new Intl.Collator('zh-CN')
const statusLabels: Record<Source['status'], string> = {
  pending: '等待同步', success: '同步正常', empty: '未识别到文章', ok: '同步正常', partial: '部分完成', error: '同步失败', unavailable: '暂无公开站点',
}
const initialOptions: SyncOptions = { sourceId: '', pagesPerChannel: 2, maxChannelsPerSource: 40 }

function readStorage<T>(key: string, fallback: T): T {
  try { return JSON.parse(localStorage.getItem(key) || 'null') ?? fallback } catch { return fallback }
}
function getInitialBookmarks(): string[] {
  const value = readStorage<unknown>('cdu-bookmarks-v1', [])
  return Array.isArray(value) ? value.filter((id): id is string => typeof id === 'string') : []
}
function getInitialOptions(): SyncOptions {
  const value = readStorage<Partial<SyncOptions>>('cdu-sync-options-v1', initialOptions)
  return {
    sourceId: '',
    pagesPerChannel: Math.min(50, Math.max(1, Number(value.pagesPerChannel) || 2)),
    maxChannelsPerSource: Math.min(100, Math.max(1, Number(value.maxChannelsPerSource) || 40)),
  }
}
function localDate(date = new Date()) {
  return `${date.getFullYear()}-${String(date.getMonth() + 1).padStart(2, '0')}-${String(date.getDate()).padStart(2, '0')}`
}
function formatTime(value: string) {
  if (!value) return '尚未同步'
  const date = new Date(value)
  if (Number.isNaN(date.getTime())) return '时间未知'
  return new Intl.DateTimeFormat('zh-CN', { month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit', hour12: false }).format(date)
}
function errorMessage(error: unknown) { return error instanceof Error ? error.message : String(error) }
function articleDay(article: Article) { return article.date ? article.date.slice(0, 10) : '' }
type SearchableArticle = Article & { content?: string; body?: string; summary?: string; excerpt?: string; description?: string }
const synonymGroups = [
  ['通知', '公告', '通告', '公示'],
  ['新闻', '动态', '资讯', '消息'],
  ['招生', '录取', '报名', '招生就业'],
  ['就业', '招聘', '求职', '实习'],
  ['教务', '教学', '课程', '课堂'],
  ['科研', '科学研究', '研究项目', '学术'],
  ['财务', '报销', '经费', '预算'],
  ['党务', '党建', '党委', '党支部'],
  ['人事', '师资', '教师', '职工'],
  ['学工', '学生工作', '学生事务', '奖助'],
  ['安全', '保卫', '消防', '安防'],
  ['国际', '外事', '留学', '海外'],
]
const synonymMap = new Map(synonymGroups.flatMap(group => group.map(term => [term, group] as const)))
function normalizeSearch(value: string) { return value.toLocaleLowerCase().replace(/[\s\u3000]+/g, '') }
// 归一化文本可以复用：调用方传入已归一化的 target/text，避免每条文章重复归一化。
function fuzzyMatchNormalized(target: string, text: string) {
  if (!target) return true
  if (text.includes(target)) return true
  let cursor = 0
  let matched = 0
  for (const char of target) {
    cursor = text.indexOf(char, cursor)
    if (cursor < 0) break
    cursor += 1
    matched += 1
  }
  if (matched === target.length) return true
  if (target.length < 3) return false
  const maxDistance = Math.max(1, Math.floor(target.length * 0.3))
  let previous = Array.from({ length: target.length + 1 }, (_, index) => index)
  for (let row = 1; row <= text.length; row += 1) {
    const current = [row]
    for (let column = 1; column <= target.length; column += 1) {
      current[column] = Math.min(current[column - 1] + 1, previous[column] + 1, previous[column - 1] + (text[row - 1] === target[column - 1] ? 0 : 1))
    }
    if (current[target.length] <= maxDistance) return true
    previous = current
  }
  return false
}
function searchTerms(query: string) { return query.trim().split(/[\s,，、]+/).map(normalizeSearch).filter(Boolean) }
const expandCache = new Map<string, string[]>()
function expandTerm(term: string) {
  const cached = expandCache.get(term)
  if (cached) return cached
  const values = [term, ...(synonymMap.get(term) || [])].filter((value, index, items) => items.indexOf(value) === index)
  expandCache.set(term, values)
  return values
}
function articleSearchText(article: Article) {
  const item = article as SearchableArticle
  return [item.title, item.sourceName, item.channel, item.dateText, item.content, item.body, item.summary, item.excerpt, item.description].filter(Boolean).join(' ')
}
// 搜索文本预索引：按文章 id 缓存「拼接 + 归一化」结果，避免每次按键对全量文章重做。
function buildSearchIndex(articles: Article[]) {
  const index = new Map<string, string>()
  for (const article of articles) index.set(article.id, normalizeSearch(articleSearchText(article)))
  return index
}
function matchesIndexedSearch(text: string, terms: string[], mode: SearchMode) {
  if (!terms.length) return true
  if (mode === 'contains') return terms.every(term => text.includes(term))
  if (mode === 'fuzzy') return terms.every(term => fuzzyMatchNormalized(term, text))
  return terms.every(term => expandTerm(term).some(candidate => fuzzyMatchNormalized(candidate, text)))
}
function exportCSV(articles: Article[]) {
  const escape = (value: string) => {
    const safe = /^[\s]*[=+@\-]/.test(value) ? `'${value}` : value
    return `"${safe.replace(/"/g, '""')}"`
  }
  const rows = [['标题', '发布日期', '原始日期', '来源', '来源类型', '栏目', '原文链接'], ...articles.map(article => [
    article.title, articleDay(article), article.dateText, article.sourceName,
    article.sourceKind === 'college' ? '学院' : '部门', article.channel, article.url,
  ])]
  const blob = new Blob(['\uFEFF' + rows.map(row => row.map(escape).join(',')).join('\r\n')], { type: 'text/csv;charset=utf-8;' })
  const url = URL.createObjectURL(blob)
  const anchor = document.createElement('a')
  anchor.href = url
  anchor.download = `成大信息集-${localDate()}.csv`
  document.body.appendChild(anchor)
  anchor.click()
  anchor.remove()
  setTimeout(() => URL.revokeObjectURL(url), 1000)
}

function Dialog({ title, subtitle, children, close }: { title: string; subtitle: string; children: ReactNode; close: () => void }) {
  const ref = useRef<HTMLDivElement>(null)
  const closeRef = useRef(close)
  closeRef.current = close
  useEffect(() => {
    const previous = document.activeElement as HTMLElement | null
    const oldOverflow = document.body.style.overflow
    document.body.style.overflow = 'hidden'
    ref.current?.focus()
    const keydown = (event: KeyboardEvent) => {
      if (event.key === 'Escape') closeRef.current()
      if (event.key !== 'Tab') return
      const nodes = ref.current?.querySelectorAll<HTMLElement>('button:not([disabled]), a[href], input:not([disabled]), select:not([disabled]), summary, [tabindex="0"]')
      if (!nodes?.length) { event.preventDefault(); return }
      const first = nodes[0], last = nodes[nodes.length - 1]
      if (event.shiftKey && (document.activeElement === first || document.activeElement === ref.current)) { event.preventDefault(); last.focus() }
      else if (!event.shiftKey && document.activeElement === last) { event.preventDefault(); first.focus() }
    }
    document.addEventListener('keydown', keydown)
    return () => { document.removeEventListener('keydown', keydown); document.body.style.overflow = oldOverflow; previous?.focus() }
  }, [])
  return <div className="dialog-backdrop" onMouseDown={event => { if (event.target === event.currentTarget) close() }}>
    <div className="dialog" ref={ref} role="dialog" aria-modal="true" aria-labelledby="dialog-title" tabIndex={-1}>
      <div className="dialog-heading"><div><p className="eyebrow">YOUR CAMPUS, IN SYNC</p><h2 id="dialog-title">{title}</h2><p>{subtitle}</p></div><button className="icon-button" aria-label="关闭弹窗" onClick={close}><Icon name="close" /></button></div>
      {children}
    </div>
  </div>
}

export default function App() {
  const [snapshot, setSnapshot] = useState<Snapshot>(emptySnapshot)
  const [loading, setLoading] = useState(true)
  const [connectionError, setConnectionError] = useState('')
  const [busy, setBusy] = useState(false)
  const [view, setView] = useState<View>('feed')
  const [kind, setKind] = useState<Kind>('all')
  const [query, setQuery] = useState('')
  const [searchMode, setSearchMode] = useState<SearchMode>('smart')
  const [channel, setChannel] = useState('all')
  const [sourceId, setSourceId] = useState('all')
  const [includedSources, setIncludedSources] = useState<string[]>([])
  const [excludedSources, setExcludedSources] = useState<string[]>([])
  const [range, setRange] = useState('all')
  const [sort, setSort] = useState('newest')
  const [page, setPage] = useState(1)
  const [bookmarks, setBookmarks] = useState<string[]>(getInitialBookmarks)
  const [options, setOptions] = useState<SyncOptions>(getInitialOptions)
  const [modal, setModal] = useState<Modal>(null)
  const [mobileNav, setMobileNav] = useState(false)
  const [toast, setToast] = useState('')
  const searchRef = useRef<HTMLInputElement>(null)
  const sidebarRef = useRef<HTMLElement>(null)
  const runningRef = useRef(false)
  // 输入框保持即时响应，实际的过滤/排序跟随延迟值执行。
  const deferredQuery = useDeferredValue(query)
  // 每次 render 只取一次「今天」，既省掉 filter 内每条文章各调一次，也让跨零点后能自愈。
  const todayDay = localDate()

  const refresh = useCallback(async () => {
    try {
      const data = await getSnapshot()
      setSnapshot(data)
      runningRef.current = data.sync.running
      setConnectionError('')
    } catch (error) { runningRef.current = false; setConnectionError(errorMessage(error)) }
    finally { setLoading(false) }
  }, [])

  useEffect(() => {
    let alive = true
    let timer: ReturnType<typeof setTimeout>
    const poll = async () => {
      await refresh()
      if (alive) timer = setTimeout(poll, runningRef.current ? 2000 : 10000)
    }
    void poll()
    return () => { alive = false; clearTimeout(timer) }
  }, [refresh])
  useEffect(() => {
    if (!toast) return
    const timer = setTimeout(() => setToast(''), 4800)
    return () => clearTimeout(timer)
  }, [toast])
  useEffect(() => {
    const keydown = (event: KeyboardEvent) => {
      if ((event.ctrlKey || event.metaKey) && event.key === 'k' && !modal) { event.preventDefault(); searchRef.current?.focus() }
    }
    window.addEventListener('keydown', keydown)
    return () => window.removeEventListener('keydown', keydown)
  }, [modal])
  useEffect(() => { setPage(1) }, [view, kind, query, searchMode, channel, sourceId, includedSources, excludedSources, range, sort])
  // 窄屏下侧栏用 transform 移出视口，但仍在 Tab 序内；关闭时用 inert + aria-hidden 把它移出可交互树。
  useEffect(() => {
    const element = sidebarRef.current
    if (!element) return
    const media = window.matchMedia('(max-width: 760px)')
    const sync = () => {
      const hidden = media.matches && !mobileNav
      element.inert = hidden
      if (hidden) element.setAttribute('aria-hidden', 'true')
      else element.removeAttribute('aria-hidden')
    }
    sync()
    media.addEventListener('change', sync)
    return () => media.removeEventListener('change', sync)
  }, [mobileNav])

  const navigate = (nextView: View, nextKind: Kind = 'all') => {
    setView(nextView); setKind(nextKind); setSourceId('all'); setIncludedSources([]); setExcludedSources([]); setChannel('all'); setRange('all'); setQuery(''); setSearchMode('smart'); setPage(1); setMobileNav(false)
    window.scrollTo({ top: 0, behavior: 'smooth' })
  }
  const toggleSourceFilter = (mode: 'include' | 'exclude', id: string) => {
    if (mode === 'include') {
      setIncludedSources(current => current.includes(id) ? current.filter(value => value !== id) : [...current, id])
      setExcludedSources(current => current.filter(value => value !== id))
    } else {
      setExcludedSources(current => current.includes(id) ? current.filter(value => value !== id) : [...current, id])
      setIncludedSources(current => current.filter(value => value !== id))
    }
  }
  const clearFilters = () => {
    setQuery(''); setSearchMode('smart'); setChannel('all'); setSourceId('all'); setIncludedSources([]); setExcludedSources([]); setRange('all'); setKind('all')
  }
  const toggleBookmark = (article: Article) => {
    const wasSaved = bookmarks.includes(article.id)
    const next = wasSaved ? bookmarks.filter(id => id !== article.id) : [...bookmarks, article.id]
    setBookmarks(next)
    try { localStorage.setItem('cdu-bookmarks-v1', JSON.stringify(next)) }
    catch { setToast('浏览器存储空间不可用，本次收藏仅在当前会话保留。'); return }
    setToast(wasSaved ? '已取消收藏' : '已加入我的收藏')
  }
  const beginSync = async (id = '') => {
    setBusy(true)
    try {
      const normalized = {
        sourceId: id,
        pagesPerChannel: Math.min(50, Math.max(1, Math.round(options.pagesPerChannel) || 2)),
        maxChannelsPerSource: Math.min(100, Math.max(1, Math.round(options.maxChannelsPerSource) || 40)),
      }
      setOptions({ ...normalized, sourceId: '' })
      await startSync(normalized)
      setToast(id ? '已开始同步这个来源' : '同步已开始，信息将陆续出现在列表中')
      await refresh()
    } catch (error) { setToast(`无法开始同步：${errorMessage(error)}`) }
    finally { setBusy(false) }
  }
  const stopSync = async () => {
    setBusy(true)
    try { await cancelSync(); setToast('已请求停止同步，已获取的信息会保留'); await refresh() }
    catch (error) { setToast(`停止失败：${errorMessage(error)}`) }
    finally { setBusy(false) }
  }
  const saveOptions = () => {
    const next = {
      sourceId: '',
      pagesPerChannel: Math.min(50, Math.max(1, Math.round(options.pagesPerChannel) || 2)),
      maxChannelsPerSource: Math.min(100, Math.max(1, Math.round(options.maxChannelsPerSource) || 40)),
    }
    setOptions(next)
    try { localStorage.setItem('cdu-sync-options-v1', JSON.stringify(next)); setToast('同步设置已保存，下次同步时生效') }
    catch { setToast('设置已应用，但浏览器无法持久保存。') }
  }
  const externalClick = (event: MouseEvent<HTMLAnchorElement>, url: string) => {
    if (!isSafeURL(url)) { event.preventDefault(); setToast('这个来源暂未提供有效的网页地址。'); return }
    if (window.go?.main?.App) {
      event.preventDefault()
      void openURL(url).catch(error => setToast(`无法打开网页：${errorMessage(error)}`))
    }
  }
  const outbound = (url: string) => ({ href: isSafeURL(url) ? url : undefined, target: '_blank', rel: 'noopener noreferrer', onClick: (event: MouseEvent<HTMLAnchorElement>) => externalClick(event, url) })

  const departments = snapshot.sources.filter(source => source.kind === 'department')
  const colleges = snapshot.sources.filter(source => source.kind === 'college')
  const channelCount = snapshot.sources.reduce((total, source) => total + source.channels.length, 0)
  const successSources = snapshot.sources.filter(source => source.status === 'success' || source.status === 'ok' || source.status === 'partial')
  const failedSources = snapshot.sources.filter(source => source.status === 'error' || source.status === 'partial')
  const channels = useMemo(() => [...new Set(snapshot.articles.filter(article => (kind === 'all' || article.sourceKind === kind) && (sourceId === 'all' || article.sourceId === sourceId)).map(article => article.channel).filter(Boolean))].sort((a, b) => zhCollator.compare(a, b)), [snapshot.articles, kind, sourceId])
  const sourceOptions = useMemo(() => snapshot.sources.filter(source => kind === 'all' || source.kind === kind).sort((a, b) => zhCollator.compare(a.name, b.name)), [snapshot.sources, kind])
  // 搜索文本预索引随文章集合变化才重建（每次 ~30 ms），按键时只做字符串包含/编辑距离比较。
  const searchIndex = useMemo(() => buildSearchIndex(snapshot.articles), [snapshot.articles])
  // 收藏只在 saved 视图参与过滤：非 saved 视图下该值为 null，点收藏不会触发全量重算。
  const savedIds = useMemo(() => (view === 'saved' ? new Set(bookmarks) : null), [view, bookmarks])
  const filteredArticles = useMemo(() => {
    const cutoff = new Date()
    if (range !== 'all') cutoff.setDate(cutoff.getDate() - Number(range) + 1)
    const cutoffDay = localDate(cutoff)
    const terms = searchTerms(deferredQuery)
    const filtered = snapshot.articles.filter(article => {
      if (savedIds && !savedIds.has(article.id)) return false
      if (kind !== 'all' && article.sourceKind !== kind) return false
      if (sourceId !== 'all' && article.sourceId !== sourceId) return false
      if (includedSources.length > 0 && !includedSources.includes(article.sourceId)) return false
      if (excludedSources.includes(article.sourceId)) return false
      if (channel !== 'all' && article.channel !== channel) return false
      if (terms.length && !matchesIndexedSearch(searchIndex.get(article.id) || '', terms, searchMode)) return false
      const day = articleDay(article)
      if (range !== 'all' && (!day || day < cutoffDay || day > todayDay)) return false
      return true
    })
    // 未来日期（常为原文标注的截止/有效期）不参与「最新」置顶：正常日期 → 日期未知 → 未来日期。
    return filtered.sort((a, b) => {
      const dayA = articleDay(a), dayB = articleDay(b)
      const tierA = !dayA ? 1 : dayA > todayDay ? 2 : 0
      const tierB = !dayB ? 1 : dayB > todayDay ? 2 : 0
      if (tierA !== tierB) return tierA - tierB
      if (dayA !== dayB) return sort === 'newest' ? (dayA < dayB ? 1 : -1) : (dayA < dayB ? -1 : 1)
      return zhCollator.compare(a.title, b.title)
    })
  }, [snapshot.articles, searchIndex, savedIds, view, kind, sourceId, includedSources, excludedSources, channel, deferredQuery, searchMode, range, sort, todayDay])
  const filteredSources = useMemo(() => {
    const terms = searchTerms(deferredQuery)
    return snapshot.sources.filter(source => {
      if (kind !== 'all' && source.kind !== kind) return false
      if (includedSources.length > 0 && !includedSources.includes(source.id)) return false
      if (excludedSources.includes(source.id)) return false
      if (!terms.length) return true
      return matchesIndexedSearch(normalizeSearch(`${source.name} ${source.group} ${source.url}`), terms, searchMode)
    })
  }, [snapshot.sources, kind, deferredQuery, searchMode, includedSources, excludedSources])
  const pageCount = Math.max(1, Math.ceil(filteredArticles.length / PAGE_SIZE))
  const currentPage = Math.min(page, pageCount)
  const visibleArticles = filteredArticles.slice((currentPage - 1) * PAGE_SIZE, currentPage * PAGE_SIZE)
  const selectedSource = snapshot.sources.find(source => source.id === sourceId)
  const hasFilters = !!query || channel !== 'all' || sourceId !== 'all' || includedSources.length > 0 || excludedSources.length > 0 || range !== 'all'
  const progress = snapshot.sync.total > 0 ? Math.min(100, Math.round(snapshot.sync.completed / snapshot.sync.total * 100)) : 0
  const coverage = snapshot.sources.length > 0 ? Math.round(successSources.length / snapshot.sources.length * 100) : 0
  const todayCount = useMemo(() => snapshot.articles.filter(article => articleDay(article) === todayDay).length, [snapshot.articles, todayDay])
  const dateLabel = new Intl.DateTimeFormat('zh-CN', { year: 'numeric', month: 'long', day: 'numeric', weekday: 'long' }).format(new Date())
  const pageTitle = view === 'sources' ? '从这里，连接每个来源。' : view === 'saved' ? '重要的信息，留在手边。' : '校园信息，一站汇集。'
  const pageSubtitle = view === 'sources' ? '一览成都大学各部门与学院，发现官网中的每一个公开栏目。' : view === 'saved' ? '为值得关注的消息留个位置，随时回看，轻松找到。' : '汇集各部门与学院的通知、动态，让每一条重要消息都不再错过。'
  const viewLabel = view === 'sources' ? '来源目录' : view === 'saved' ? '我的收藏' : kind === 'department' ? '部门动态' : kind === 'college' ? '学院资讯' : '信息总览'

  const navItem = (label: string, icon: IconName, targetView: View, targetKind: Kind = 'all', count?: number) => {
    const active = view === targetView && (targetView !== 'feed' || kind === targetKind)
    return <button className={`nav-item ${active ? 'active' : ''}`} onClick={() => navigate(targetView, targetKind)} aria-current={active ? 'page' : undefined}><Icon name={icon} size={19}/><span>{label}</span>{count !== undefined && <span className="nav-count">{number.format(count)}</span>}</button>
  }
  const kindTabs = <div className="kind-tabs" role="group" aria-label="信息来源类型">{([
    ['all', '全部信息', snapshot.sources.length], ['department', '学校部门', departments.length], ['college', '院系学院', colleges.length],
  ] as const).map(([value, label, count]) => <button key={value} className={kind === value ? 'selected' : ''} aria-pressed={kind === value} onClick={() => { setKind(value); setSourceId('all'); setIncludedSources([]); setExcludedSources([]); setChannel('all') }}>{label}<span>{count}</span></button>)}</div>

  return <div className="app-shell">
    {mobileNav && <button className="sidebar-backdrop" onClick={() => setMobileNav(false)} aria-label="收起导航"/>}
    <aside className={`sidebar ${mobileNav ? 'is-open' : ''}`} aria-label="主导航" ref={sidebarRef}>
      <a className="brand" href="#" onClick={event => { event.preventDefault(); navigate('feed') }}><span className="brand-mark"><Icon name="graduation" size={27}/></span><span><strong>成大<span> · </span>信息集</strong><small>CDU INFORMATION HUB</small></span></a>
      <div className="sidebar-divider"/>
      <p className="nav-label">校园工作台 <span>WORKSPACE</span></p>
      <nav>
        {navItem('信息总览', 'grid', 'feed')}
        {navItem('部门动态', 'building', 'feed', 'department')}
        {navItem('学院资讯', 'graduation', 'feed', 'college')}
        {navItem('我的收藏', 'bookmark', 'saved', 'all', bookmarks.length || undefined)}
        <div className="nav-space"/>
        <p className="nav-label">数据与来源</p>
        {navItem('来源目录', 'globe', 'sources')}
        <button className="nav-item" onClick={() => { setModal('sync'); setMobileNav(false) }}><Icon name="refresh" size={19}/><span>同步管理</span>{snapshot.sync.running && <span className="nav-live"/>}</button>
      </nav>
      <div className="sidebar-bottom">
        <div className="sidebar-note"><span className="tiny-tag">保持连接</span><p>散落的信息，<br/>在这里有了交集。</p><span className="note-line"/><span className="note-circle"/></div>
        <button className="sidebar-about" onClick={() => { setModal('about'); setMobileNav(false) }}><span className="about-circle"><Icon name="info" size={18}/></span><span>关于信息集<small>校园公开信息聚合</small></span><Icon name="chevron" size={16}/></button>
      </div>
    </aside>

    <div className="main-shell">
      <header className="topbar"><div className="breadcrumb"><button className="icon-button mobile-menu" aria-label="打开导航" onClick={() => setMobileNav(true)}><Icon name="menu"/></button><span>校园工作台</span><Icon name="chevron" size={13}/><strong>{viewLabel}</strong></div><div className="topbar-right"><span className="topbar-date"><Icon name="sun" size={16}/>{dateLabel}</span><span className="topbar-divider"/><a {...outbound('https://www.cdu.edu.cn/')} className="official-link">成都大学官网<Icon name="external" size={14}/></a></div></header>
      <main id="main-content">
        <section className="hero"><div><p className="eyebrow"><span/>PUBLIC INFORMATION, CONNECTED</p><h1>{pageTitle}</h1><p className="hero-description">{pageSubtitle}</p></div><button className={`button button-primary sync-main ${snapshot.sync.running ? 'is-syncing' : ''}`} disabled={busy || loading || !!connectionError} onClick={() => snapshot.sync.running ? setModal('sync') : void beginSync()}><Icon name="refresh" size={17} className={snapshot.sync.running ? 'spin' : ''}/>{snapshot.sync.running ? '正在同步' : '同步最新信息'}{!snapshot.sync.running && <Icon name="arrow" size={17}/>}</button></section>

        {connectionError && <div className="connection-error" role="alert"><Icon name="warning"/><div><strong>暂时无法连接信息服务</strong><span>{connectionError}。已加载的信息仍可浏览。</span></div><button className="button button-outline" onClick={() => void refresh()}>重新连接</button></div>}

        <section className="stats-grid" aria-label="信息统计">
          {([
            { label: '已汇集信息', value: snapshot.articles.length, icon: 'file', note: todayCount ? `今日发布 ${number.format(todayCount)} 条` : '公开信息，持续归集', kind: 'articles' },
            { label: '学校部门', value: departments.length, icon: 'building', note: '来自学校组织机构目录', kind: 'departments' },
            { label: '院系学院', value: colleges.length, icon: 'graduation', note: '来自学校院系设置目录', kind: 'colleges' },
            { label: '发现栏目', value: channelCount, icon: 'layers', note: '通知公告 · 新闻动态 · 更多', kind: 'channels' },
          ] as const).map((stat, index) => <article className={`stat-card stat-${index}`} key={stat.kind}><div className="stat-label"><span>{stat.label}</span><span className="stat-icon"><Icon name={stat.icon} size={19}/></span></div><div className="stat-value">{loading ? <span className="skeleton stat-skeleton"/> : number.format(stat.value)}<span>{index === 0 ? '条' : '个'}</span></div><p>{stat.note}</p></article>)}
        </section>

        <div className={`workspace-grid ${view === 'sources' ? 'directory-layout' : ''}`}>
          <section className="feed-panel">
            <div className="panel-heading"><div className="section-title"><span className="section-mark"/><h2>{view === 'sources' ? '探索信息来源' : view === 'saved' ? '我的收藏' : '校园信息流'}</h2><span className="section-count">{number.format(view === 'sources' ? filteredSources.length : filteredArticles.length)} {view === 'sources' ? '个来源' : '条信息'}</span></div>{view !== 'sources' && <button className="text-button export-button" onClick={() => { exportCSV(filteredArticles); setToast(`已导出 ${filteredArticles.length} 条信息`) }} disabled={!filteredArticles.length}><Icon name="download" size={15}/>导出 CSV</button>}</div>
            <div className="filters">
              {kindTabs}
              <div className="search-row"><label className="search-field"><Icon name="search" size={18}/><input ref={searchRef} type="search" value={query} onChange={event => setQuery(event.target.value)} placeholder={view === 'sources' ? '搜索部门、学院或站点…' : searchMode === 'smart' ? '智能搜索标题、来源与正文…' : '搜索标题、来源或关键词…'} aria-label={view === 'sources' ? '搜索信息来源' : '搜索信息'}/><kbd>⌘ / Ctrl K</kbd></label><label className="search-mode-select"><span>搜索</span><select value={searchMode} onChange={event => setSearchMode(event.target.value as SearchMode)} aria-label="搜索模式"><option value="smart">智能同义词</option><option value="fuzzy">模糊匹配</option><option value="contains">包含匹配</option></select></label>{view !== 'sources' && <label className="sort-select"><Icon name="clock" size={15}/><select value={sort} onChange={event => setSort(event.target.value)} aria-label="排序方式"><option value="newest">最新发布</option><option value="oldest">最早发布</option></select></label>}</div>
              <div className="filter-row">{view !== 'sources' && <><label><span>栏目</span><select value={channel} onChange={event => setChannel(event.target.value)} aria-label="筛选栏目"><option value="all">全部栏目</option>{channels.map(item => <option key={item} value={item}>{item}</option>)}</select></label><label><span>时间</span><select value={range} onChange={event => setRange(event.target.value)} aria-label="筛选发布时间"><option value="all">全部时间</option><option value="1">今日</option><option value="7">最近 7 天</option><option value="30">最近 30 天</option><option value="90">最近 90 天</option></select></label><label className="source-select"><span>来源</span><select value={sourceId} onChange={event => { setSourceId(event.target.value); setChannel('all') }} aria-label="筛选具体来源"><option value="all">全部来源</option>{sourceOptions.map(source => <option key={source.id} value={source.id}>{source.name}</option>)}</select></label></>}<details className="source-filter-popover"><summary><Icon name="layers" size={14}/>来源范围{includedSources.length + excludedSources.length > 0 && <b>{includedSources.length + excludedSources.length}</b>}</summary><div className="source-filter-panel"><div className="source-filter-column"><div className="source-filter-heading"><strong>只看</strong><span>包含来源</span></div><div className="source-filter-list">{sourceOptions.map(source => <label key={`include-${source.id}`}><input type="checkbox" checked={includedSources.includes(source.id)} onChange={() => toggleSourceFilter('include', source.id)}/><span>{source.name}</span></label>)}</div></div><div className="source-filter-column"><div className="source-filter-heading"><strong>不看</strong><span>排除来源</span></div><div className="source-filter-list">{sourceOptions.map(source => <label key={`exclude-${source.id}`}><input type="checkbox" checked={excludedSources.includes(source.id)} onChange={() => toggleSourceFilter('exclude', source.id)}/><span>{source.name}</span></label>)}</div></div></div></details>{hasFilters && <button className="clear-filters" onClick={clearFilters}>重置<Icon name="close" size={12}/></button>}</div>
            </div>

            {view === 'sources' ? <>
              <div className="directory-note"><Icon name="info" size={15}/><span>自动读取学校官方目录。展开来源，可查看发现的子栏目。</span></div>
              {loading ? <LoadingRows/> : filteredSources.length ? <div className="source-grid">{filteredSources.map(source => <article className="source-card" key={source.id}><div className="source-card-top"><span className={`source-avatar ${source.kind === 'college' ? 'college-avatar' : ''}`}><Icon name={source.kind === 'college' ? 'graduation' : 'building'} size={22}/></span><span className={`status-pill status-${source.status}`}><span/>{statusLabels[source.status] || '等待同步'}</span></div><h3>{source.name}</h3><p className="source-group">{source.group || (source.kind === 'college' ? '院系学院' : '学校部门')}</p><div className="source-facts"><span><strong>{number.format(source.articleCount)}</strong> 条信息</span><span><strong>{source.channels.length}</strong> 个栏目</span></div><div className="source-card-actions"><button onClick={() => { navigate('feed', source.kind); setSourceId(source.id) }}>查看信息<Icon name="arrow" size={14}/></button><a {...outbound(source.url)} aria-disabled={!isSafeURL(source.url)} tabIndex={isSafeURL(source.url) ? 0 : -1} title={source.url || '官方目录暂未提供网站链接'}>官网<Icon name="external" size={13}/></a><button className="source-refresh" aria-label={`同步${source.name}`} title="同步这个来源" disabled={snapshot.sync.running || busy || !isSafeURL(source.url) || !!connectionError} onClick={() => void beginSync(source.id)}><Icon name="refresh" size={15}/></button></div>{source.error && <details className="source-error"><summary><Icon name="warning" size={12}/>查看采集说明</summary><p>{source.error}</p></details>}<details className="channel-details"><summary>发现的栏目<span>{source.channels.length}<Icon name="chevronDown" size={13}/></span></summary>{source.channels.length ? <ul>{source.channels.map(item => <li key={item.id || item.url}><a {...outbound(item.url)}>{item.name}<Icon name="external" size={11}/></a></li>)}</ul> : <p>{source.status === 'unavailable' ? '官方目录未提供可访问的网站链接。' : '同步后，发现的公开栏目将显示在这里。'}</p>}</details><p className="source-updated">最近采集 · {formatTime(source.lastCrawled)}</p></article>)}</div> : <EmptyState icon="globe" title={query ? '没有找到匹配的来源' : '从学校官网发现信息来源'} description={query ? '试试部门或学院名称中的其他关键词。' : '开始同步后，将从组织机构和院系设置目录发现各个站点。'} action={!query ? <button className="button button-primary" disabled={busy || snapshot.sync.running || !!connectionError} onClick={() => void beginSync()}><Icon name="refresh" size={16}/>发现来源</button> : <button className="button button-outline" onClick={() => setQuery('')}>清除搜索</button>}/>}<div className="source-directory-links"><span>目录依据</span><a {...outbound('https://www.cdu.edu.cn/zzjg.htm')}>组织机构<Icon name="external" size={12}/></a><a {...outbound('https://www.cdu.edu.cn/yxsz.htm')}>院系设置<Icon name="external" size={12}/></a></div>
            </> : <>
              {selectedSource && <div className="active-source"><span>正在浏览：{selectedSource.name}</span><button onClick={() => { setSourceId('all'); setChannel('all') }} aria-label="取消来源筛选"><Icon name="close" size={13}/></button></div>}
              <div className="list-header"><span>标题 / 来源</span><span>发布日期</span></div>
              {loading ? <LoadingRows/> : visibleArticles.length ? <div className="article-list">{visibleArticles.map(article => {
                const saved = bookmarks.includes(article.id)
                const day = articleDay(article)
                const isToday = day === todayDay
                const isFuture = !!day && day > todayDay
                return <article className="article-row" key={article.id}><span className={`article-dot ${article.sourceKind === 'college' ? 'college-dot' : ''}`}/><div className="article-main"><div className="article-title-wrap"><a className="article-title" {...outbound(article.url)}>{article.title}</a>{isToday && <span className="new-badge">今日</span>}</div><div className="article-meta"><button className={article.sourceKind === 'college' ? 'college-meta' : ''} onClick={() => { setSourceId(article.sourceId); setKind(article.sourceKind); setChannel('all') }}>{article.sourceName}</button><span className="meta-divider"/><button onClick={() => setChannel(article.channel)}>{article.channel || '未分类'}</button></div></div><div className="article-date"><time dateTime={article.date || undefined} title={isFuture ? '该日期晚于今天，可能是原文标注的截止日期' : undefined}>{day || '日期未标注'}</time>{!article.date && article.dateText ? <span title={article.dateText}>{article.dateText}</span> : null}</div><button className={`bookmark-button ${saved ? 'is-saved' : ''}`} aria-label={`${saved ? '取消收藏' : '收藏'}：${article.title}`} aria-pressed={saved} onClick={() => toggleBookmark(article)}><Icon name="bookmark" size={17}/></button></article>
              })}</div> : <EmptyState icon={view === 'saved' ? 'bookmark' : hasFilters ? 'search' : 'layers'} title={view === 'saved' && !hasFilters ? '把值得关注的信息收藏在这里' : hasFilters ? '没有找到匹配的信息' : snapshot.sync.running ? '正在汇集校园信息' : '校园信息，从第一次同步开始'} description={view === 'saved' && !hasFilters ? '点击信息右侧的书签，即可加入收藏。收藏记录保存在当前设备。' : hasFilters ? '试试更简短的关键词，或放宽栏目、时间与来源筛选条件。' : snapshot.sync.running ? '正在逐个访问学校部门与学院网站，获取标题、日期和原文链接。' : '连接成都大学的部门与学院网站，发现通知公告、新闻动态等公开栏目。'} action={hasFilters ? <button className="button button-outline" onClick={clearFilters}>清除筛选</button> : view === 'saved' ? <button className="button button-outline" onClick={() => navigate('feed')}>去浏览校园信息<Icon name="arrow" size={16}/></button> : <button className="button button-primary" disabled={busy || snapshot.sync.running || !!connectionError} onClick={() => void beginSync()}><Icon name="refresh" size={16} className={snapshot.sync.running ? 'spin' : ''}/>{snapshot.sync.running ? '同步进行中…' : '开始首次同步'}</button>}/>}<div className="pagination"><span>共 {number.format(filteredArticles.length)} 条{filteredArticles.length > 0 && <>，显示 {(currentPage - 1) * PAGE_SIZE + 1}–{Math.min(currentPage * PAGE_SIZE, filteredArticles.length)}</>}</span><div><button disabled={currentPage <= 1} aria-label="上一页" onClick={() => setPage(currentPage - 1)}><Icon name="chevron" size={14} className="rotate-180"/></button><span><strong>{currentPage}</strong> / {pageCount}</span><button disabled={currentPage >= pageCount} aria-label="下一页" onClick={() => setPage(currentPage + 1)}><Icon name="chevron" size={14}/></button></div></div>
            </>}
          </section>

          <aside className="insights-column" aria-label="信息来源与同步概况">
            <section className="sync-card"><div className="small-card-heading"><h2>同步概况</h2><span className={`sync-state ${snapshot.sync.running ? 'running' : connectionError ? 'disconnected' : ''}`}><span/>{connectionError ? '服务未连接' : snapshot.sync.running ? '同步中' : '等待下一次更新'}</span></div><div className="sync-illustration" aria-hidden="true"><div className="orbit orbit-one"/><div className="orbit orbit-two"/><div className="orbit-dot dot-one"/><div className="orbit-dot dot-two"/><div className="sync-orb"><Icon name={snapshot.sync.running ? 'refresh' : 'layers'} size={28} className={snapshot.sync.running ? 'spin' : ''}/></div></div><p className="sync-card-title">{snapshot.sync.running ? '校园消息，正在汇合' : snapshot.lastUpdated ? '信息已就绪，随时探索' : '与校园消息保持同步'}</p><p className="sync-card-description">{snapshot.sync.running ? snapshot.sync.current || '正在发现网站和栏目…' : `最近更新 · ${formatTime(snapshot.lastUpdated)}`}</p>{snapshot.sync.running && <div className="progress-section"><div className="progress-label"><span>来源处理进度</span><strong>{snapshot.sync.completed} / {snapshot.sync.total || '—'}</strong></div><div className="progress-track" role="progressbar" aria-label="同步来源进度" aria-valuenow={progress} aria-valuemin={0} aria-valuemax={100}><span style={{ width: `${Math.max(progress, 2)}%` }}/></div></div>}<button className="button sync-card-button" onClick={() => setModal('sync')}>{snapshot.sync.running ? '查看同步进度' : '管理同步与采集'}<Icon name="arrow" size={16}/></button></section>

            <section className="coverage-card"><div className="small-card-heading"><h2>来源覆盖</h2><button className="text-button" onClick={() => navigate('sources')}>查看全部<Icon name="chevron" size={13}/></button></div><div className="coverage-total"><strong>{successSources.length}<span> / {snapshot.sources.length}</span></strong><span>已采集来源</span></div><div className="coverage-track" role="img" aria-label={`已采集 ${coverage}% 的来源`}><span style={{ width: `${coverage}%` }}/></div><div className="coverage-types"><button onClick={() => navigate('sources', 'department')}><span className="legend-dot"/>学校部门<strong>{departments.length}</strong></button><button onClick={() => navigate('sources', 'college')}><span className="legend-dot college-dot"/>院系学院<strong>{colleges.length}</strong></button></div>{failedSources.length > 0 && <button className="coverage-warning" onClick={() => setModal('sync')}><Icon name="warning" size={14}/>{failedSources.length} 个来源有采集说明<Icon name="chevron" size={13}/></button>}</section>

            <section className="reading-note"><span className="reading-note-icon"><Icon name="info" size={19}/></span><h3>让信息有迹可循</h3><p>每条信息都保留发布来源与原文链接。点击标题，即可前往官网查看完整内容。</p><div className="reading-note-footer"><span>公开来源</span><span>原文可溯</span><span>本地收藏</span></div></section>
            <div className="right-column-footer"><span className="footer-cross">✳</span><p>看见校园的每一次更新。<small>MADE FOR YOUR CAMPUS LIFE</small></p></div>
          </aside>
        </div>
        <footer className="page-footer"><span>成大 · 信息集 <span className="footer-dot">·</span> 连接校园，汇集日常</span><span>黎明制作 · 仅聚合公开信息 · 非学校官方网站</span></footer>
      </main>
    </div>

    {modal === 'sync' && <Dialog title="同步管理" subtitle="从官方目录发现来源，按设定范围获取各站点的公开信息。" close={() => setModal(null)}><div className="dialog-body"><div className="modal-status"><span className={`modal-status-icon ${snapshot.sync.running ? 'running' : ''}`}><Icon name={snapshot.sync.running ? 'refresh' : 'layers'} size={24} className={snapshot.sync.running ? 'spin' : ''}/></span><div><strong>{snapshot.sync.running ? '正在同步校园信息' : '随时可以开始新的同步'}</strong><p>{snapshot.sync.running ? snapshot.sync.current || '正在发现来源…' : `最近完成：${formatTime(snapshot.sync.finishedAt)}`}</p></div></div>{snapshot.sync.running && <div className="modal-progress"><div className="progress-label"><span>{snapshot.sync.phase || '同步中'}</span><strong>{snapshot.sync.completed} / {snapshot.sync.total || '—'}</strong></div><div className="progress-track"><span style={{ width: `${Math.max(progress, 2)}%` }}/></div></div>}<div className="settings-heading"><Icon name="settings" size={18}/><h3>采集范围</h3></div><div className="settings-grid"><label><span>每个栏目采集页数</span><div className="number-input"><input type="number" min={1} max={50} step={1} value={options.pagesPerChannel} disabled={snapshot.sync.running} onChange={event => setOptions({ ...options, pagesPerChannel: Number(event.target.value) })}/><span>页</span></div><small>1–50 页，默认 2 页</small></label><label><span>每个来源最多栏目数</span><div className="number-input"><input type="number" min={1} max={100} step={1} value={options.maxChannelsPerSource} disabled={snapshot.sync.running} onChange={event => setOptions({ ...options, maxChannelsPerSource: Number(event.target.value) })}/><span>个</span></div><small>1–100 个，默认 40 个</small></label></div><p className="settings-help"><Icon name="info" size={16}/><span>采集范围为有限的历史页面；增大页数会延长同步时间。日期缺失会明确标注，保留原文月日，请以原文为准。已获取的信息在停止同步后仍会保留。</span></p><button className="text-button settings-save" disabled={snapshot.sync.running} onClick={saveOptions}><Icon name="check" size={15}/>保存采集设置</button><div className="modal-metrics"><span>已发现来源<strong>{snapshot.sources.length}</strong></span><span>已发现栏目<strong>{channelCount}</strong></span><span>已汇集信息<strong>{number.format(snapshot.articles.length)}</strong></span></div>{(snapshot.sync.errors.length > 0 || failedSources.length > 0) && <details className="sync-errors"><summary><Icon name="warning" size={16}/>同步说明与失败记录<span>{snapshot.sync.errors.length + failedSources.length}<Icon name="chevronDown" size={14}/></span></summary><ul>{snapshot.sync.errors.map((error, index) => <li key={`sync-${index}`}>{error}</li>)}{failedSources.map(source => <li key={source.id}><strong>{source.name}</strong>{source.error || '部分内容未能获取，请重试。'}</li>)}</ul></details>}</div><div className="dialog-actions"><button className="button button-outline" onClick={() => setModal(null)}>返回浏览</button>{snapshot.sync.running ? <button className="button button-danger" disabled={busy} onClick={() => void stopSync()}><Icon name="stop" size={16}/>{busy ? '正在停止…' : '停止同步'}</button> : <button className="button button-primary" disabled={busy || loading || !!connectionError} onClick={() => void beginSync()}><Icon name="refresh" size={16}/>{busy ? '正在启动…' : '开始同步全部来源'}</button>}</div></Dialog>}
    {modal === 'about' && <Dialog title="成大 · 信息集" subtitle="把散落在校园各处的公开消息，汇集到一个安静、有序的地方。" close={() => setModal(null)}><div className="dialog-body about-body"><div className="about-brand"><Icon name="graduation" size={36}/><span>校园信息，一站汇集。</span></div><p>信息集通过成都大学官方组织机构与院系设置页面发现各部门、学院的公开网站，自动识别通知公告、新闻动态等栏目，保存信息标题、日期与原文链接。</p><p>它是独立的信息聚合工具，并非成都大学官方网站。信息以原发布网站为准，公开目录中的失效或未提供链接会在来源目录中标明。收藏保存在当前浏览器或桌面应用的本地存储中。</p><div className="about-links"><a {...outbound('https://www.cdu.edu.cn/zzjg.htm')}><Icon name="building" size={17}/>组织机构目录<Icon name="external" size={13}/></a><a {...outbound('https://www.cdu.edu.cn/yxsz.htm')}><Icon name="graduation" size={17}/>院系设置目录<Icon name="external" size={13}/></a></div></div><div className="dialog-actions"><button className="button button-primary" onClick={() => setModal(null)}>开始探索<Icon name="arrow" size={16}/></button></div></Dialog>}
    {toast && <div className="toast" role="status"><span>{toast}</span><button aria-label="关闭提示" onClick={() => setToast('')}><Icon name="close" size={15}/></button></div>}
  </div>
}

function EmptyState({ icon, title, description, action }: { icon: IconName; title: string; description: string; action: ReactNode }) {
  return <div className="empty-state"><div className="empty-orbit"><span/><Icon name={icon} size={30}/></div><h3>{title}</h3><p>{description}</p>{action}</div>
}
function LoadingRows() {
  return <div className="loading-rows" role="status" aria-label="正在读取信息">{Array.from({ length: 5 }, (_, index) => <div className="loading-row" key={index}><div><span className="skeleton" style={{ width: `${78 - index * 7}%` }}/><span className="skeleton" style={{ width: '36%' }}/></div><span className="skeleton"/></div>)}</div>
}
