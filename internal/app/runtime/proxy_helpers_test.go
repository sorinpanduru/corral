package runtime_test

import (
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.mongodb.org/mongo-driver/v2/bson"

	corralrt "github.com/sorinpanduru/corral/internal/app/runtime"
	"github.com/sorinpanduru/corral/internal/testkit/rawwire"
)

func bsonD(kv ...any) bson.D {
	var d bson.D
	for i := 0; i+1 < len(kv); i += 2 {
		d = append(d, bson.E{Key: kv[i].(string), Value: kv[i+1]})
	}
	return d
}

func bsonHello() bson.D { return bsonD("hello", 1, "$db", "admin") }

func bsonFind(marker string) bson.D {
	return bsonD("find", "c", "$db", "d", "corraltestMarker", marker)
}

func sendRecv(c *rawwire.Conn, raw []byte) (*rawwire.Message, error) {
	if err := c.Send(raw); err != nil {
		return nil, err
	}
	return c.ReadMessage(10 * time.Second)
}

func sendRecvOK(t *testing.T, c *rawwire.Conn, raw []byte) *rawwire.Message {
	t.Helper()
	m, err := sendRecv(c, raw)
	if err != nil {
		t.Fatalf("round trip: %v", err)
	}
	return m
}

func gaugeValue(t *testing.T, hd corralrt.Handles, name string) float64 {
	t.Helper()
	fams, err := hd.Gatherer.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range fams {
		if f.GetName() == name && len(f.GetMetric()) > 0 && f.GetMetric()[0].GetGauge() != nil {
			return f.GetMetric()[0].GetGauge().GetValue()
		}
	}
	return -1
}

var _ prometheus.Gatherer

type recordingProbe struct {
	mu   sync.Mutex
	pre  int
	held int
}

func (p *recordingProbe) ReadBeforeLease(id int32, n int) {
	p.mu.Lock()
	if n > p.pre {
		p.pre = n
	}
	p.mu.Unlock()
}
func (p *recordingProbe) ReplyHeld(id int32, n int) {
	p.mu.Lock()
	if n > p.held {
		p.held = n
	}
	p.mu.Unlock()
}
func (p *recordingProbe) preLease() int { p.mu.Lock(); defer p.mu.Unlock(); return p.pre }
