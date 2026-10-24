package tracing

import (
	"context"
	"net/url"
	"strings"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.33.0"
)

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
func NewResource(ctx context.Context, env LookupEnv) (*resource.Resource, error) {
	det, err := resource.New(ctx, resource.WithTelemetrySDK(), resource.WithHost())
	if err != nil {
		return nil, err
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
				continue // malformed pairs are ignored, as the OTel spec says
			}
			v = strings.TrimSpace(v)
			if dec, derr := url.PathUnescape(v); derr == nil {
				v = dec
			}
			attrs[attribute.Key(k)] = attribute.String(k, v)
		}
	}

	if _, ok := attrs[semconv.ServiceNameKey]; !ok {
		attrs[semconv.ServiceNameKey] = semconv.ServiceName(defaultServiceName)
	}
	if v, ok := env("OTEL_SERVICE_NAME"); ok && strings.TrimSpace(v) != "" {
		attrs[semconv.ServiceNameKey] = semconv.ServiceName(strings.TrimSpace(v))
	}

	out := make([]attribute.KeyValue, 0, len(attrs))
	for k, kv := range attrs {
		if _, banned := bannedResourceKeys[k]; banned {
			continue
		}
		out = append(out, kv)
	}
	return resource.NewWithAttributes(semconv.SchemaURL, out...), nil
}
