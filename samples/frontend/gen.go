// Package frontend is an example golite app with a server-rendered frontend
// in app/: templ components, htmx for partial page updates, and Tailwind CSS
// built by golite's pinned standalone CLI, over sqlc queries on SQLite. Run
// `go generate` after editing queries/, a .templ file or a Tailwind class.
package frontend

// Generate Models
//go:generate go tool sqlc generate

// Generate Components
//go:generate go tool templ generate -path app

// Generate CSS (downloads tailwindcss into ./bin on first run)
//go:generate go tool tailwind -i app/styles.css -o app/static/styles.css --minify
