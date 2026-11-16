import cytoscape from 'cytoscape'
import { useEffect, useRef } from 'react'
import type { Graph } from './api'
import { navigate } from './router'

// Colores por tecnología (se adaptan al tema por CSS variables leídas en tiempo de dibujo).
const techVar: Record<string, string> = {
  Virtualización: '--t1',
  Balanceadores: '--t2',
  Cabinas: '--t3',
  Firewalls: '--t4',
  Backups: '--t5',
}

function cssVar(name: string): string {
  return getComputedStyle(document.documentElement).getPropertyValue(name).trim() || '#888'
}

// Dibuja el grafo en columnas por distancia al origen: a la derecha lo que necesita
// (dependencias) y a la izquierda lo que depende de él o lo publica.
export default function GraphView({ graph, rootIds, height = 460 }: { graph: Graph; rootIds: string[]; height?: number }) {
  const ref = useRef<HTMLDivElement>(null)
  useEffect(() => {
    if (!ref.current) return
    const roots = new Set(rootIds)
    const colOf = (n: Graph['nodes'][number]) =>
      roots.has(n.id) || n.side === 'self' ? 0 : n.side === 'down' || n.side === 'publishes' ? -n.depth : n.depth
    const cols = new Map<number, number>()
    const pos = new Map<string, { x: number; y: number }>()
    const sorted = [...graph.nodes].sort((a, b) => (a.class + a.name).localeCompare(b.class + b.name))
    for (const n of sorted) {
      const c = colOf(n)
      const row = cols.get(c) || 0
      cols.set(c, row + 1)
      pos.set(n.id, { x: c * 260, y: row * 56 })
    }
    for (const [c, rows] of cols) {
      for (const n of sorted) if (colOf(n) === c) pos.get(n.id)!.y -= ((rows - 1) * 56) / 2
    }
    const ink = cssVar('--ink')
    const muted = cssVar('--muted')
    const line = cssVar('--line')
    const cy = cytoscape({
      container: ref.current,
      elements: [
        ...graph.nodes.map((n) => ({
          data: {
            id: n.id,
            label: n.name.length > 34 ? n.name.slice(0, 33) + '…' : n.name,
            cls: n.class,
            color: cssVar(techVar[n.technology] || '--t0'),
            root: roots.has(n.id) ? 1 : 0,
            off: n.lifecycle !== 'active' ? 1 : 0,
          },
          position: pos.get(n.id),
        })),
        ...graph.edges
          .filter((e) => pos.has(e.src) && pos.has(e.dst))
          .map((e) => ({ data: { id: 'e' + e.id, source: e.src, target: e.dst, label: e.rel, same: e.rel === 'es el mismo equipo que' ? 1 : 0 } })),
      ],
      layout: { name: 'preset' },
      wheelSensitivity: 0.25,
      style: [
        {
          selector: 'node',
          style: {
            shape: 'round-rectangle',
            width: 200,
            height: 38,
            'background-color': 'data(color)',
            'background-opacity': 0.14,
            'border-width': 1.2,
            'border-color': 'data(color)',
            label: (ele: cytoscape.NodeSingular) => ele.data('cls') + '\n' + ele.data('label'),
            'text-wrap': 'wrap',
            'text-valign': 'center',
            'font-size': 10.5,
            color: ink,
          },
        },
        { selector: 'node[root = 1]', style: { 'border-width': 3, 'background-opacity': 0.3 } },
        { selector: 'node[off = 1]', style: { 'border-style': 'dashed', opacity: 0.6 } },
        {
          selector: 'edge',
          style: {
            width: 1,
            'line-color': line,
            'target-arrow-color': line,
            'target-arrow-shape': 'triangle',
            'arrow-scale': 0.8,
            'curve-style': 'bezier',
            label: 'data(label)',
            'font-size': 8.5,
            color: muted,
            'text-rotation': 'autorotate',
            'text-background-color': cssVar('--card'),
            'text-background-opacity': 1,
            'text-background-padding': '1px',
          },
        },
        { selector: 'edge[same = 1]', style: { 'line-style': 'dashed', 'target-arrow-shape': 'none' } },
      ],
    })
    cy.on('tap', 'node', (ev) => navigate('/ci/' + ev.target.id()))
    cy.fit(undefined, 24)
    if (cy.zoom() > 1.1) cy.zoom(1.1)
    cy.center()
    return () => cy.destroy()
  }, [graph, rootIds])
  return <div ref={ref} className="graph" style={{ height }} />
}
