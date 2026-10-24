// Package metrics holds the corral metric catalog (FR-31), its lint (FR-10,
// FR-11, FR-13, FR-29), a catalog-backed metric factory and the private
// Prometheus registry (FR-9, FR-14). See docs/adr/0004-metric-catalog.md.
package metrics

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// NameTracingSpansDropped is the FR-37 counter.
const NameTracingSpansDropped = "corral_tracing_spans_dropped_total"

// Type is the metric type of a catalog entry.
type Type string

// Supported metric types.
const (
	TypeCounter   Type = "counter"
	TypeGauge     Type = "gauge"
	TypeHistogram Type = "histogram"
)

// Catalog is the machine-readable metric catalog.
type Catalog struct {
	Metrics []MetricDef `json:"metrics"`
}

// MetricDef describes one metric.
type MetricDef struct {
	Name    string     `json:"name"`
	Type    Type       `json:"type"`
	Help    string     `json:"help"`
	Labels  []LabelDef `json:"labels"`
	Buckets []float64  `json:"buckets,omitempty"`
}

// LabelDef describes one label and its closed set of allowed values.
type LabelDef struct {
	Name   string   `json:"name"`
	Values []string `json:"values"`
}

//go:embed catalog.json
var embeddedCatalog []byte

// LoadCatalog parses the embedded catalog.json.
func LoadCatalog() (*Catalog, error) {
	return ParseCatalog(embeddedCatalog)
}

// ParseCatalog strictly parses a catalog document: unknown fields, malformed
// JSON and trailing data are errors. It does not lint.
func ParseCatalog(data []byte) (*Catalog, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var c Catalog
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("metrics: parse catalog: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("metrics: parse catalog: trailing data after document")
	}
	return &c, nil
}
