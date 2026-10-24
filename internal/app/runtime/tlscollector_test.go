package runtime_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	collectortrace "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/protobuf/proto"
)

// tlsCollector is a minimal TLS OTLP collector (the otlpcollector testkit is
// plaintext only): an HTTPS endpoint for OTLP/HTTP and a TLS gRPC endpoint,
// both with a server certificate that chains to CAFile (a synthetic CA, SAN
// 127.0.0.1). It counts spans received. Synthetic material only.
type tlsCollector struct {
	CAFile   string
	HTTPAddr string
	GRPCAddr string
	spans    atomic.Int64
}

func (c *tlsCollector) Spans() int { return int(c.spans.Load()) }

type tlsGRPC struct {
	collectortrace.UnimplementedTraceServiceServer
	c *tlsCollector
}

func (g tlsGRPC) Export(_ context.Context, r *collectortrace.ExportTraceServiceRequest) (*collectortrace.ExportTraceServiceResponse, error) {
	g.c.count(r)
	return &collectortrace.ExportTraceServiceResponse{}, nil
}

func (c *tlsCollector) count(r *collectortrace.ExportTraceServiceRequest) {
	for _, rs := range r.GetResourceSpans() {
		for _, ss := range rs.GetScopeSpans() {
			c.spans.Add(int64(len(ss.GetSpans())))
		}
	}
}

func startTLSCollector(t *testing.T) *tlsCollector {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "corral-test-ca.invalid"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, caKey.Public(), caKey)
	if err != nil {
		t.Fatal(err)
	}
	caCert, _ := x509.ParseCertificate(caDER)
	srvKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	srvTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "corral-test-srv.invalid"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
	}
	srvDER, err := x509.CreateCertificate(rand.Reader, srvTmpl, caCert, srvKey.Public(), caKey)
	if err != nil {
		t.Fatal(err)
	}
	tlsCfg := &tls.Config{
		Certificates: []tls.Certificate{{Certificate: [][]byte{srvDER}, PrivateKey: srvKey}},
		NextProtos:   []string{"h2", "http/1.1"},
	}
	c := &tlsCollector{CAFile: filepath.Join(t.TempDir(), "ca.crt")}
	if err := os.WriteFile(c.CAFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}), 0o600); err != nil {
		t.Fatal(err)
	}

	hl, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/traces", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(io.LimitReader(r.Body, 8<<20))
		var req collectortrace.ExportTraceServiceRequest
		if proto.Unmarshal(b, &req) == nil {
			c.count(&req)
		}
		out, _ := proto.Marshal(&collectortrace.ExportTraceServiceResponse{})
		w.Header().Set("Content-Type", "application/x-protobuf")
		_, _ = w.Write(out)
	})
	hs := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = hs.Serve(tls.NewListener(hl, tlsCfg)) }()
	c.HTTPAddr = hl.Addr().String()

	gl, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	gs := grpc.NewServer(grpc.Creds(credentials.NewTLS(tlsCfg)))
	collectortrace.RegisterTraceServiceServer(gs, tlsGRPC{c: c})
	go func() { _ = gs.Serve(gl) }()
	c.GRPCAddr = gl.Addr().String()

	t.Cleanup(func() { gs.Stop(); _ = hs.Close() })
	return c
}
