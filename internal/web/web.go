// Package web embeds the browser-facing assets and templates so the binary
// ships its UI with no build step, and serves them with immutable caching.
package web

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"html/template"
	"io/fs"
	"net/http"
	"sort"
	"strings"
)

//go:embed static/*
var staticFS embed.FS

//go:embed templates/*.html
var templateFS embed.FS

// Version is a short content hash of every static file. Templates append it
// as ?v= so a deploy invalidates browser caches without any build tooling.
var Version = version()

func version() string {
	h := sha256.New()
	names, _ := fs.Glob(staticFS, "static/*")
	sort.Strings(names)
	for _, n := range names {
		b, _ := staticFS.ReadFile(n)
		h.Write([]byte(n))
		h.Write(b)
	}
	return hex.EncodeToString(h.Sum(nil))[:8]
}

// Shell is what every layout needs regardless of page.
type Shell struct {
	Title   string // rendered as "<Title> · Entropix"; "" (e.g. the homepage) renders bare "Entropix"
	Version string // web.Version, for cache-busting static URLs
	Email   string // signed-in developer, "" when anonymous
	CSRF    string // logout form; "" on public pages
	Nav     string // "dashboard" | "mail" | "chat" | "docs" | "" for aria-current
}

// Static serves /static/{file}. Everything under it is content-addressed by
// Version, so it is safe to cache forever — but only once we know the file
// actually exists: setting the immutable header unconditionally would have a
// 404 for a missing or mistyped asset cached by the browser (and any shared
// cache in front of it) for a year too.
func Static() http.Handler {
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		panic(err)
	}
	files := http.FileServer(http.FS(sub))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "..") {
			http.NotFound(w, r)
			return
		}
		name := strings.TrimPrefix(r.URL.Path, "/static/")
		info, err := fs.Stat(sub, name)
		if err != nil || info.IsDir() {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		http.StripPrefix("/static/", files).ServeHTTP(w, r)
	})
}

var tmpl = template.Must(template.New("").Funcs(template.FuncMap{
	"lower": strings.ToLower,
	"initial": func(s string) string {
		if s == "" {
			return "?"
		}
		return strings.ToUpper(s[:1])
	},
}).ParseFS(templateFS, "templates/*.html"))

// Templates is the parsed set. Pages are executed by name ("dashboard",
// "mail", ...); each page {{template}}s one of the three layouts.
func Templates() *template.Template { return tmpl }
