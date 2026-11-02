package main

import (
	"embed"
	"encoding/json"
	"net/http"
	"strings"

	"bba.local/taskengine/internal/taskengine"
)

//go:embed product/*
var productAssets embed.FS

func systemSettingsSurface(next http.Handler, registry *taskengine.ModelRegistry, auth func(*http.Request) (taskengine.Actor, error), store *taskengine.Store) http.Handler {
	proxy := registry.ProxySettingsHandler(auth, store)
	search := registry.SearchSettingsHandler(auth, store)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case taskengine.WebSearchPath, taskengine.WebSearchPath + "/test":
			search.ServeHTTP(w, r)
		case taskengine.GlobalProxyPath:
			proxy.ServeHTTP(w, r)
		default:
			next.ServeHTTP(w, r)
		}
	})
}

func productSurface(api, dashboard http.Handler, auth func(*http.Request) (taskengine.Actor, error)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/operator/dashboard" {
			dashboard.ServeHTTP(w, r)
			return
		}
		if r.URL.Path == "/v1/operator/tools" {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Cache-Control", "no-store")
			a, e := auth(r)
			if e != nil || a.WorkspaceID == "" || a.PrincipalID == "" {
				w.WriteHeader(401)
				_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": "UNAUTHENTICATED"}})
				return
			}
			if a.RuntimeID != "" || !a.Permissions["operator.read"] {
				w.WriteHeader(403)
				return
			}
			if r.Method != "GET" {
				w.Header().Set("Allow", "GET")
				w.WriteHeader(405)
				return
			}
			raw, e := productAssets.ReadFile("product/tools.json")
			if e != nil {
				http.Error(w, "registry unavailable", 503)
				return
			}
			_, _ = w.Write(raw)
			return
		}
		if r.URL.Path == "/product" {
			http.Redirect(w, r, "/product/", 307)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/product/") {
			if r.Method != "GET" {
				w.Header().Set("Allow", "GET")
				w.WriteHeader(405)
				return
			}
			file := "index.html"
			kind := "text/html; charset=utf-8"
			switch r.URL.Path {
			case "/product/app.css":
				file = "app.css"
				kind = "text/css; charset=utf-8"
			case "/product/app.js":
				file = "app.js"
				kind = "text/javascript; charset=utf-8"
			default:
				if strings.Contains(strings.TrimPrefix(r.URL.Path, "/product/"), ".") {
					http.NotFound(w, r)
					return
				}
			}
			body, e := productAssets.ReadFile("product/" + file)
			if e != nil {
				http.Error(w, "asset unavailable", 500)
				return
			}
			w.Header().Set("Content-Type", kind)
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("X-Content-Type-Options", "nosniff")
			w.Header().Set("Content-Security-Policy", "default-src 'self'; connect-src 'self'; style-src 'self'; script-src 'self'; img-src 'self' data:; base-uri 'none'; frame-ancestors 'none'")
			_, _ = w.Write(body)
			return
		}
		api.ServeHTTP(w, r)
	})
}
