package mongosim

import (
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// Role is the server role advertised in hello replies.
type Role int

const (
	// Standalone is a server with no replica set (isWritablePrimary, no setName).
	Standalone Role = iota
	// Primary is a replica-set primary.
	Primary
	// Secondary is a replica-set secondary.
	Secondary
	// Arbiter is a replica-set arbiter.
	Arbiter
	// Mongos is a sharded-cluster router (msg: isdbgrid).
	Mongos
	// Other is a replica-set member that is neither primary, secondary nor
	// arbiter (RSOther: for example hidden or recovering).
	Other
)

// Defaults applied when a HelloSpec field is zero.
const (
	DefaultMaxWire             = 21 // MongoDB 7.0
	DefaultMaxBsonObjectSize   = 16777216
	DefaultMaxMessageSizeBytes = 48000000
	DefaultMaxWriteBatchSize   = 100000
	DefaultLogicalSessionMins  = 30
)

// HelloSpec describes what the server answers to hello / isMaster / ismaster.
type HelloSpec struct {
	Role     Role
	SetName  string
	Hosts    []string
	Passives []string
	Arbiters []string
	// Primary is the "primary" field (host:port). Empty omits it.
	Primary string
	// Me overrides the "me" field. Empty uses the server's own address for
	// replica-set roles and omits it for Standalone and Mongos.
	Me string
	// ElectionID is 12 bytes; it is sent only for the Primary role. Empty omits it.
	ElectionID []byte
	// SetVersion is sent when non-zero.
	SetVersion int32

	MinWire                      int32
	MaxWire                      int32 // 0 means DefaultMaxWire
	MaxBsonObjectSize            int32
	MaxMessageSizeBytes          int32
	MaxWriteBatchSize            int32
	LogicalSessionTimeoutMinutes int32 // 0 means DefaultLogicalSessionMins; negative omits the field

	// Extra fields are appended to the reply verbatim (they win over nothing:
	// duplicates are the test's responsibility).
	Extra bson.D
}

func (h HelloSpec) maxMsg() int32 {
	if h.MaxMessageSizeBytes == 0 {
		return DefaultMaxMessageSizeBytes
	}
	return h.MaxMessageSizeBytes
}

// helloDoc builds the reply body for spec, as seen by connection c.
func (s *Server) helloDoc(h HelloSpec, counter int64, connID int, helloOK, legacy bool, mechs bson.A) bson.D {
	isPrimary := h.Role == Primary || h.Role == Standalone || h.Role == Mongos
	d := bson.D{}
	if helloOK {
		d = append(d, bson.E{Key: "helloOk", Value: true})
	}
	// hello replies carry isWritablePrimary only; legacy isMaster replies
	// carry ismaster only (as real servers do).
	if legacy {
		d = append(d, bson.E{Key: "ismaster", Value: isPrimary})
	} else {
		d = append(d, bson.E{Key: "isWritablePrimary", Value: isPrimary})
	}
	rs := h.Role == Primary || h.Role == Secondary || h.Role == Arbiter || h.Role == Other
	if rs {
		if h.SetName != "" {
			d = append(d, bson.E{Key: "setName", Value: h.SetName})
		}
		if h.SetVersion != 0 {
			d = append(d, bson.E{Key: "setVersion", Value: h.SetVersion})
		}
		d = append(d, bson.E{Key: "secondary", Value: h.Role == Secondary})
		if h.Role == Arbiter {
			d = append(d, bson.E{Key: "arbiterOnly", Value: true})
		}
		if len(h.Hosts) > 0 {
			d = append(d, bson.E{Key: "hosts", Value: strArray(h.Hosts)})
		}
		if len(h.Passives) > 0 {
			d = append(d, bson.E{Key: "passives", Value: strArray(h.Passives)})
		}
		if len(h.Arbiters) > 0 {
			d = append(d, bson.E{Key: "arbiters", Value: strArray(h.Arbiters)})
		}
		if h.Primary != "" {
			d = append(d, bson.E{Key: "primary", Value: h.Primary})
		}
		me := h.Me
		if me == "" {
			me = s.Addr()
		}
		d = append(d, bson.E{Key: "me", Value: me})
		if h.Role == Primary && len(h.ElectionID) == 12 {
			var oid bson.ObjectID
			copy(oid[:], h.ElectionID)
			d = append(d, bson.E{Key: "electionId", Value: oid})
		}
		if h.Role == Other {
			d = append(d, bson.E{Key: "hidden", Value: true})
		}
	} else if h.Me != "" {
		d = append(d, bson.E{Key: "me", Value: h.Me})
	}
	if h.Role == Mongos {
		d = append(d, bson.E{Key: "msg", Value: "isdbgrid"})
	}
	mbo := h.MaxBsonObjectSize
	if mbo == 0 {
		mbo = DefaultMaxBsonObjectSize
	}
	mwb := h.MaxWriteBatchSize
	if mwb == 0 {
		mwb = DefaultMaxWriteBatchSize
	}
	maxWire := h.MaxWire
	if maxWire == 0 {
		maxWire = DefaultMaxWire
	}
	d = append(d,
		bson.E{Key: "maxBsonObjectSize", Value: mbo},
		bson.E{Key: "maxMessageSizeBytes", Value: h.maxMsg()},
		bson.E{Key: "maxWriteBatchSize", Value: mwb},
		bson.E{Key: "localTime", Value: bson.NewDateTimeFromTime(time.Now())},
	)
	switch {
	case h.LogicalSessionTimeoutMinutes == 0:
		d = append(d, bson.E{Key: "logicalSessionTimeoutMinutes", Value: int32(DefaultLogicalSessionMins)})
	case h.LogicalSessionTimeoutMinutes > 0:
		d = append(d, bson.E{Key: "logicalSessionTimeoutMinutes", Value: h.LogicalSessionTimeoutMinutes})
	}
	d = append(d,
		bson.E{Key: "connectionId", Value: int32(connID + 1)},
		bson.E{Key: "minWireVersion", Value: h.MinWire},
		bson.E{Key: "maxWireVersion", Value: maxWire},
		bson.E{Key: "readOnly", Value: false},
		bson.E{Key: "topologyVersion", Value: bson.D{
			{Key: "processId", Value: s.processID},
			{Key: "counter", Value: counter},
		}},
	)
	if mechs != nil {
		d = append(d, bson.E{Key: "saslSupportedMechs", Value: mechs})
	}
	d = append(d, h.Extra...)
	d = append(d, bson.E{Key: "ok", Value: float64(1)})
	return d
}

func strArray(ss []string) bson.A {
	a := make(bson.A, len(ss))
	for i, s := range ss {
		a[i] = s
	}
	return a
}
