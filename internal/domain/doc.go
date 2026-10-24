// Package domain is the root of corral's domain layer.
//
// FR-1 rule: packages under this path may import only the Go standard library
// (excluding log/slog), other packages under internal/domain, and an explicit
// allowlist that is currently empty. Domain code never imports application,
// infrastructure or test-kit packages.
package domain
