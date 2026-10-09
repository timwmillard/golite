// Package multitenant is an example golite app where each tenant gets its
// own SQLite database, alongside a master database listing the tenants.
// Like samples/todo it uses an OpenAPI spec (oapi-codegen strict server)
// and sqlc queries. Run `go generate` after editing api/spec.yaml or the
// queries.
package multitenant

// Generate Models
//go:generate go tool sqlc generate

// Generate Server
//go:generate go tool oapi-codegen -config api/server.yaml api/spec.yaml
