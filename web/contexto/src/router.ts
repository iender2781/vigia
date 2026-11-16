// Enrutador mínimo basado en la URL (history API). El servidor devuelve index.html para
// cualquier ruta que no sea un fichero, así que los enlaces se pueden compartir.
import { useEffect, useState } from 'react'

export function navigate(to: string) {
  if (to === location.pathname + location.search) return
  history.pushState(null, '', to)
  window.dispatchEvent(new PopStateEvent('popstate'))
  window.scrollTo(0, 0)
}

export function useLocation(): URL {
  const [url, setUrl] = useState(() => new URL(location.href))
  useEffect(() => {
    const on = () => setUrl(new URL(location.href))
    window.addEventListener('popstate', on)
    return () => window.removeEventListener('popstate', on)
  }, [])
  return url
}

export function onLinkClick(e: React.MouseEvent<HTMLAnchorElement>) {
  const a = e.currentTarget
  if (e.metaKey || e.ctrlKey || e.shiftKey || e.button !== 0 || a.target === '_blank') return
  const href = a.getAttribute('href')
  if (!href || !href.startsWith('/')) return
  e.preventDefault()
  navigate(href)
}
