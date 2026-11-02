package main

import (
	"embed"
	"net/http"
)

//go:embed web/index.html web/app.css web/app.js
var consoleAssets embed.FS

func serveConsoleAsset(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodGet {
		return false
	}

	asset, ok := map[string][2]string{
		"/":        {"web/index.html", "text/html; charset=utf-8"},
		"/app.css": {"web/app.css", "text/css; charset=utf-8"},
		"/app.js":  {"web/app.js", "text/javascript; charset=utf-8"},
	}[r.URL.Path]
	if !ok {
		return false
	}
	body, err := consoleAssets.ReadFile(asset[0])
	if err != nil {
		http.Error(w, "console asset unavailable", http.StatusInternalServerError)
		return true
	}
	w.Header().Set("Content-Type", asset[1])
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; connect-src 'self'; style-src 'self'; script-src 'self'; img-src 'self' data:; base-uri 'none'; frame-ancestors 'none'")
	_, _ = w.Write(body)
	return true
}

func consoleSurface(api http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if serveConsoleAsset(w, r) {
			return
		}
		api.ServeHTTP(w, r)
	})
}
