// Package todo is an example golite app with an OpenAPI spec (oapi-codegen
// strict server) and sqlc queries. Run `go generate` after editing
// api/spec.yaml or queries/.
package todo

// Generate Models
//go:generate go tool sqlc generate

// Generate Server
//go:generate go tool oapi-codegen -config api/server.yaml api/spec.yaml
