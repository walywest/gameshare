package main

import (
	_ "embed"
	"net/http"
)

// The homelab page: services, updates, and whatever the services add. It's
// built into the binary so it always matches the API it talks to.
//
//go:embed web/index.html
var pageHTML []byte

func handlePage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(pageHTML)
}
