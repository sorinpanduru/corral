package tracing

import (
	"context"
	"errors"
	"net/url"
	"strings"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.33.0"
)

// varServiceName is the OTEL_SERVICE_NAME variable (FR-20, FR-30).
const varServiceName = "OTEL_SERVICE_NAME"

// errResourceDetect is the constant sentinel for detector failures; the
// underlying error text is never propagated.
var errResourceDetect = errors.New("tracing: resource detection failed")

// defaultServiceName is the service.name used when the operator sets none.
const defaultServiceName = "corral"

// bannedResourceKeys are never emitted, whatever their source (FR-30).
var bannedResourceKeys = map[attribute.Key]struct{}{
	"process.command_args": {},
	"process.command_line": {},
	"process.owner":        {},
}

// NewResource builds the span resource (FR-30).
//
// Compliance (FR-24, GDPR data minimisation): only the service.*,
// telemetry.sdk.* and host.name detector output is kept; the process.*
// attributes that can carry command lines or user names are removed even when
// the operator supplies them. Environment is read only through env, never
// through resource.WithFromEnv or os.Getenv. Values are never echoed in errors.
//
// IMPORTANT for T15: sdktrace.WithResource merges resource.Environment(), which
// reads the real process environment, so the banned process.* keys can come
// back into exported spans. The exporter handed to the span processor MUST be
// wrapped with FilterResource(exp, res) so exported spans carry exactly res.
func NewResource(ctx context.Context, env LookupEnv) (*resource.Resource, error) {
	det, err := resource.New(ctx, resource.WithTelemetrySDK(), resource.WithHost())
	if err != nil && !errors.Is(err, resource.ErrPartialResource) {
		return nil, errResourceDetect
	}
	// On ErrPartialResource keep the partial detector resource; host.name is
	// simply absent if the host detector failed.
	if det == nil {
		det = resource.Empty()
	}
	attrs := map[attribute.Key]attribute.KeyValue{}
	for _, kv := range det.Attributes() {
		k := string(kv.Key)
		if strings.HasPrefix(k, "service.") || strings.HasPrefix(k, "telemetry.sdk.") || k == "host.name" {
			attrs[kv.Key] = kv
		}
	}

	if raw, ok := env(varResourceAttrs); ok {
		for _, part := range strings.Split(raw, ",") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			k, v, found := strings.Cut(part, "=")
			k = strings.TrimSpace(k)
			if !found || k == "" {
				continue // defensive: Resolve (T10) rejects malformed lists first
			}
			v = strings.TrimSpace(v)
			if dec, derr := url.PathUnescape(v); derr == nil {
				v = dec
			}
			if isBannedKey(k) {
				continue
			}
			attrs[attribute.Key(k)] = attribute.String(k, v)
		}
	}

	if _, ok := attrs[semconv.ServiceNameKey]; !ok {
		attrs[semconv.ServiceNameKey] = semconv.ServiceName(defaultServiceName)
	}
	if v, ok := env(varServiceName); ok && strings.TrimSpace(v) != "" {
		attrs[semconv.ServiceNameKey] = semconv.ServiceName(strings.TrimSpace(v))
	}

	out := make([]attribute.KeyValue, 0, len(attrs))
	for k, kv := range attrs {
		if isBannedKey(string(k)) {
			continue
		}
		out = append(out, kv)
	}
	return resource.NewWithAttributes(semconv.SchemaURL, out...), nil
}

// isBannedKey compares both the raw key and its percent-decoded form.
func isBannedKey(k string) bool {
	if _, ok := bannedResourceKeys[attribute.Key(k)]; ok {
		return true
	}
	if dec, err := url.PathUnescape(k); err == nil {
		if _, ok := bannedResourceKeys[attribute.Key(dec)]; ok {
			return true
		}
	}
	return false
}
