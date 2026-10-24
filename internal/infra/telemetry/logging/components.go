// Package logging provides the only way to obtain a corral logger (SPEC-001
// FR-4..FR-8, FR-38, FR-39). Output goes through log/slog, synchronously.
package logging

// Component is the value of the mandatory component attribute (FR-7).
type Component string

// The fixed component set (FR-7, with thirdparty added per plan decision D5).
const (
	Config     Component = "config"
	Lifecycle  Component = "lifecycle"
	AdminHTTP  Component = "admin_http"
	Tracing    Component = "tracing"
	OTel       Component = "otel"
	GRPC       Component = "grpc"
	ThirdParty Component = "thirdparty"
	Proxy      Component = "proxy"
	Pool       Component = "pool"
	Upstream   Component = "upstream"
)

// Components returns a fresh copy of the fixed component set.
func Components() []Component {
	return []Component{Config, Lifecycle, AdminHTTP, Tracing, OTel, GRPC, ThirdParty, Proxy, Pool, Upstream}
}

// ValidComponent reports whether c is in the fixed set.
func ValidComponent(c Component) bool {
	switch c {
	case Config, Lifecycle, AdminHTTP, Tracing, OTel, GRPC, ThirdParty, Proxy, Pool, Upstream:
		return true
	}
	return false
}
