import { useCallback, useEffect, useState } from 'react'
import { api, getToken, setToken } from './api'
import { AuthContext, Link, RefreshContext } from './common'
import CIDetail from './pages/CIDetail'
import { Quality, Runs } from './pages/Ops'
import Overview from './pages/Overview'
import Search from './pages/Search'
import { SSIIDetail, SSIIList } from './pages/SSII'
import { navigate, useLocation } from './router'

type Theme = 'light' | 'dark' | ''

function useTheme(): [Theme, (t: Theme) => void] {
  const [t, setT] = useState<Theme>(() => {
    try {
      return (localStorage.getItem('contexto.theme') as Theme) || ''
    } catch {
      return ''
    }
  })
  useEffect(() => {
    if (t) document.documentElement.dataset.theme = t
    else delete document.documentElement.dataset.theme
    try {
      if (t) localStorage.setItem('contexto.theme', t)
      else localStorage.removeItem('contexto.theme')
    } catch {
      /* sin almacenamiento */
    }
  }, [t])
  return [t, setT]
}

function TokenDialog({ onDone }: { onDone: () => void }) {
  const [v, setV] = useState('')
  return (
    <div className="modal">
      <form
        className="card"
        onSubmit={(e) => {
          e.preventDefault()
          setToken(v.trim())
          onDone()
        }}
      >
        <h2>Acceso</h2>
        <p className="muted">Esta instalación pide un token de API. Pídelo al equipo de operación.</p>
        <input type="password" value={v} onChange={(e) => setV(e.target.value)} placeholder="Token" autoFocus />
        <button>Entrar</button>
      </form>
    </div>
  )
}

export default function App() {
  const url = useLocation()
  const [tick, setTick] = useState(0)
  const [needToken, setNeedToken] = useState(false)
  const [theme, setTheme] = useTheme()
  const [who, setWho] = useState<{ user: string; tenant: string; version: string } | null>(null)
  const [q, setQ] = useState('')
  const onAuth = useCallback(() => setNeedToken(true), [])

  useEffect(() => {
    api<{ user: string; tenant: string; version: string }>('/whoami')
      .then(setWho)
      .catch(() => setNeedToken(true))
  }, [tick])

  // Canal en vivo: cuando termina un ciclo de descubrimiento se refrescan los datos.
  useEffect(() => {
    if (needToken) return
    const tok = getToken()
    const es = new EventSource('/api/v1/stream' + (tok ? '?access_token=' + encodeURIComponent(tok) : ''))
    es.addEventListener('discovery', () => setTick((t) => t + 1))
    return () => es.close()
  }, [needToken])

  const path = url.pathname
  let page
  let m: RegExpMatchArray | null
  if ((m = path.match(/^\/ci\/([^/]+)$/))) page = <CIDetail id={decodeURIComponent(m[1])} />
  else if (path === '/ci') page = <Search url={url} />
  else if ((m = path.match(/^\/ssii\/(.+)$/))) page = <SSIIDetail code={decodeURIComponent(m[1])} />
  else if (path === '/ssii') page = <SSIIList />
  else if (path === '/calidad') page = <Quality />
  else if (path === '/descubrimiento') page = <Runs />
  else page = <Overview />

  const nav = [
    ['/', 'Resumen'],
    ['/ci', 'Elementos'],
    ['/ssii', 'Sistemas de Información'],
    ['/calidad', 'Calidad de datos'],
    ['/descubrimiento', 'Descubrimiento'],
  ]
  return (
    <AuthContext.Provider value={onAuth}>
      <RefreshContext.Provider value={tick}>
        <header className="top">
          <Link to="/" className="brand">
            Vigía <span>· Contexto</span>
          </Link>
          <nav>
            {nav.map(([to, label]) => (
              <Link key={to} to={to} className={(to === '/' ? path === '/' : path.startsWith(to)) ? 'on' : ''}>
                {label}
              </Link>
            ))}
          </nav>
          <form
            className="quick"
            onSubmit={(e) => {
              e.preventDefault()
              if (q.trim()) navigate('/ci?q=' + encodeURIComponent(q.trim()))
            }}
          >
            <input value={q} onChange={(e) => setQ(e.target.value)} placeholder="Buscar elemento o COD_SSII" />
          </form>
          <button
            className="theme"
            title="Cambiar tema"
            onClick={() => setTheme(theme === '' ? 'dark' : theme === 'dark' ? 'light' : '')}
          >
            Tema: {theme === 'dark' ? 'oscuro' : theme === 'light' ? 'claro' : 'auto'}
          </button>
        </header>
        <main>{page}</main>
        <footer className="muted small">
          {who ? (
            <>
              {who.tenant} · {who.user} · contexto {who.version}
            </>
          ) : null}
        </footer>
        {needToken && (
          <TokenDialog
            onDone={() => {
              setNeedToken(false)
              setTick((t) => t + 1)
            }}
          />
        )}
      </RefreshContext.Provider>
    </AuthContext.Provider>
  )
}
