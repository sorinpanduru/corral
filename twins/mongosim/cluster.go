package mongosim

import (
	"encoding/binary"
	"sync"
	"testing"
)

// ClusterOptions configures StartReplicaSet.
type ClusterOptions struct {
	// SetName defaults to "rs0".
	SetName string
	// Arbiters is the number of extra arbiter members (not counted in n).
	Arbiters int
	// Passives is how many of the last data-bearing members are advertised in
	// "passives" instead of "hosts".
	Passives int
	// Users and RequireAuth apply to every member.
	Users       map[string]string
	RequireAuth bool
	// MinWire, MaxWire apply to every member (0 = defaults).
	MinWire, MaxWire int32
	// NoInitialPrimary starts with no primary elected.
	NoInitialPrimary bool
}

// Cluster is a simulated replica set whose members advertise each other's
// real addresses.
type Cluster struct {
	mu         sync.Mutex
	opts       ClusterOptions
	data       []*Server
	arbiters   []*Server
	primary    int // -1 none
	electionN  uint32
	setVersion int32
}

// StartReplicaSet starts n data-bearing members (plus opts.Arbiters arbiters).
// Member 0 is the initial primary unless opts.NoInitialPrimary.
func StartReplicaSet(tb testing.TB, n int, opts ClusterOptions) *Cluster {
	tb.Helper()
	if opts.SetName == "" {
		opts.SetName = "rs0"
	}
	c := &Cluster{opts: opts, primary: -1, setVersion: 1}
	mk := func() *Server {
		return Start(tb, Options{Users: opts.Users, RequireAuth: opts.RequireAuth})
	}
	for i := 0; i < n; i++ {
		c.data = append(c.data, mk())
	}
	for i := 0; i < opts.Arbiters; i++ {
		c.arbiters = append(c.arbiters, mk())
	}
	if !opts.NoInitialPrimary && n > 0 {
		c.mu.Lock()
		c.primary = 0
		c.electionN++
		c.mu.Unlock()
	}
	c.publish()
	return c
}

// Member returns data-bearing member i.
func (c *Cluster) Member(i int) *Server { return c.data[i] }

// Arbiter returns arbiter i.
func (c *Cluster) Arbiter(i int) *Server { return c.arbiters[i] }

// Len is the number of data-bearing members.
func (c *Cluster) Len() int { return len(c.data) }

// Addrs lists data-bearing member addresses in member order.
func (c *Cluster) Addrs() []string {
	out := make([]string, len(c.data))
	for i, s := range c.data {
		out[i] = s.Addr()
	}
	return out
}

// URI is a driver connection string for the set (no credentials).
func (c *Cluster) URI() string {
	u := "mongodb://"
	for i, a := range c.Addrs() {
		if i > 0 {
			u += ","
		}
		u += a
	}
	return u + "/?replicaSet=" + c.opts.SetName
}

// SetPrimary elects member i: its electionId is bumped and the others become
// secondaries.
func (c *Cluster) SetPrimary(i int) {
	c.mu.Lock()
	c.primary = i
	c.electionN++
	c.mu.Unlock()
	c.publish()
}

// NoPrimary leaves every member a secondary with no primary advertised.
func (c *Cluster) NoPrimary() {
	c.mu.Lock()
	c.primary = -1
	c.mu.Unlock()
	c.publish()
}

// StepDown makes member i a secondary. If it was the primary the set has no
// primary until SetPrimary.
func (c *Cluster) StepDown(i int) {
	c.mu.Lock()
	if c.primary == i {
		c.primary = -1
	}
	c.mu.Unlock()
	c.publish()
}

// BumpSetVersion increments setVersion on every member.
func (c *Cluster) BumpSetVersion() {
	c.mu.Lock()
	c.setVersion++
	c.mu.Unlock()
	c.publish()
}

// PrimaryIndex returns the current primary's index or -1.
func (c *Cluster) PrimaryIndex() int { c.mu.Lock(); defer c.mu.Unlock(); return c.primary }

// publish pushes the current topology to every member.
func (c *Cluster) publish() {
	c.mu.Lock()
	primary, n, sv := c.primary, c.electionN, c.setVersion
	c.mu.Unlock()
	var hosts, passives, arbs []string
	firstPassive := len(c.data) - c.opts.Passives
	for i, s := range c.data {
		if i >= firstPassive && c.opts.Passives > 0 {
			passives = append(passives, s.Addr())
		} else {
			hosts = append(hosts, s.Addr())
		}
	}
	for _, s := range c.arbiters {
		arbs = append(arbs, s.Addr())
	}
	pAddr := ""
	if primary >= 0 {
		pAddr = c.data[primary].Addr()
	}
	eid := make([]byte, 12)
	copy(eid, []byte{0x7f, 0xff, 0xff, 0xff})
	binary.BigEndian.PutUint32(eid[8:], n)
	for i, s := range c.data {
		role := Secondary
		if i == primary {
			role = Primary
		}
		s.SetHello(HelloSpec{
			Role: role, SetName: c.opts.SetName, Hosts: hosts, Passives: passives, Arbiters: arbs,
			Primary: pAddr, ElectionID: eid, SetVersion: sv,
			MinWire: c.opts.MinWire, MaxWire: c.opts.MaxWire,
		})
	}
	for _, s := range c.arbiters {
		s.SetHello(HelloSpec{
			Role: Arbiter, SetName: c.opts.SetName, Hosts: hosts, Passives: passives, Arbiters: arbs,
			Primary: pAddr, SetVersion: sv, MinWire: c.opts.MinWire, MaxWire: c.opts.MaxWire,
		})
	}
}
