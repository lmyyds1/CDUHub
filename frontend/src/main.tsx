import React from 'react'
import ReactDOM from 'react-dom/client'
import App from './App'
import './styles.css'

// 渲染期异常兜底：避免整页白屏，给出中文提示与重载入口。
class ErrorBoundary extends React.Component<{ children: React.ReactNode }, { failed: boolean }> {
  state = { failed: false }
  static getDerivedStateFromError() { return { failed: true } }
  componentDidCatch(error: unknown) { console.error('界面渲染异常：', error) }
  render() {
    if (!this.state.failed) return this.props.children
    return <div className="crash-screen" role="alert">
      <h1>界面出错，请刷新</h1>
      <p>页面遇到了无法处理的异常，已加载的信息不会丢失。</p>
      <button className="button button-primary" onClick={() => window.location.reload()}>刷新页面</button>
    </div>
  }
}

ReactDOM.createRoot(document.getElementById('root')!).render(
  <React.StrictMode><ErrorBoundary><App /></ErrorBoundary></React.StrictMode>,
)
