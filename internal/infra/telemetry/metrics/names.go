package metrics

// Metric names added by SPEC-002 (FR-48). The catalog is the source of truth;
// these constants only avoid string literals at call sites.
const (
	NameClientConnections              = "corral_client_connections"
	NameClientConnectionsAcceptedTotal = "corral_client_connections_accepted_total"
	NameClientConnectionsClosedTotal   = "corral_client_connections_closed_total"
	NameClientAuthRejectedTotal        = "corral_client_auth_rejected_total"
	NameCommandsBlockedTotal           = "corral_commands_blocked_total"
	NameHandshakesTotal                = "corral_handshakes_total"
	NameProtocolErrorsTotal            = "corral_protocol_errors_total"
	NameRequestsTotal                  = "corral_requests_total"
	NameRequestDurationSeconds         = "corral_request_duration_seconds"
	NameRequestErrorsTotal             = "corral_request_errors_total"
	NameRequestsDroppedTotal           = "corral_requests_dropped_total"
	NamePoolConnections                = "corral_pool_connections"
	NamePoolMaxConnections             = "corral_pool_max_connections"
	NamePoolWaiters                    = "corral_pool_waiters"
	NamePoolCheckoutsTotal             = "corral_pool_checkouts_total"
	NamePoolCheckoutFailuresTotal      = "corral_pool_checkout_failures_total"
	NamePoolCheckoutWaitSeconds        = "corral_pool_checkout_wait_seconds"
	NameUpstreamConnectionsOpenedTotal = "corral_upstream_connections_opened_total"
	NameUpstreamConnectionsClosedTotal = "corral_upstream_connections_closed_total"
	NameUpstreamConnectErrorsTotal     = "corral_upstream_connect_errors_total"
	NameUpstreamAuthFailuresTotal      = "corral_upstream_auth_failures_total"
	NameUpstreamAvailable              = "corral_upstream_available"
	NameUpstreamPrimaryChangesTotal    = "corral_upstream_primary_changes_total"
)
