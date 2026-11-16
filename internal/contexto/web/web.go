// Package web sirve la aplicación React compilada (web/contexto, salida en dist/).
// Si no se ha compilado, sirve una página que lo indica: la API sigue funcionando.
package web

import (
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

//go:embed all:dist
var distFS embed.FS

const notBuilt = `<!doctype html><html lang="es"><meta charset="utf-8"><title>Contexto</title>
<body style="font-family:system-ui;margin:3rem;max-width:40rem">
<h1>Web no compilada</h1>
<p>Este binario se compiló sin la web. Ejecuta <code>make web</code> antes de <code>make build</code>,
o usa la imagen de contenedor oficial. La API está disponible en <a href="/api/v1/stats">/api/v1</a>.</p>
</body></html>`

// Handler sirve los ficheros estáticos y, para cualquier otra ruta, index.html (SPA).
func Handler() http.Handler {
	sub, _ := fs.Sub(distFS, "dist")
	index, err := fs.ReadFile(sub, "index.html")
	if err != nil {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Write([]byte(notBuilt))
		})
	}
	files := http.FileServer(http.FS(sub))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if p != "" && p != "index.html" {
			if _, err := fs.Stat(sub, p); err == nil {
				if strings.HasPrefix(p, "assets/") {
					w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
				}
				files.ServeHTTP(w, r)
				return
			}
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		w.Write(index)
	})
}
