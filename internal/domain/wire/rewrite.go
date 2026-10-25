package wire

import "encoding/binary"

// RewriteRequestHeader returns the 20 bytes (header plus flags) of a request
// forwarded upstream: a fresh requestID, responseTo 0, the checksum bit and
// exhaustAllowed cleared, and the length reduced by 4 when a checksum was
// present (it is stripped, FR-10, FR-30). hdr must hold at least 20 bytes and
// is not modified.
func RewriteRequestHeader(hdr []byte, newID int32) (out [20]byte, newLength int32) {
	copy(out[:], hdr[:20])
	length := int32(binary.LittleEndian.Uint32(out[0:]))
	flags := binary.LittleEndian.Uint32(out[16:])
	if flags&FlagChecksumPresent != 0 {
		length -= 4
	}
	flags &^= FlagChecksumPresent | FlagExhaustAllowed
	binary.LittleEndian.PutUint32(out[0:], uint32(length))
	binary.LittleEndian.PutUint32(out[4:], uint32(newID))
	binary.LittleEndian.PutUint32(out[8:], 0)
	binary.LittleEndian.PutUint32(out[16:], flags)
	return out, length
}

// RewriteReplyHeader returns the 20 header bytes of a reply sent to a client:
// a fresh requestID, responseTo set to the client's requestID, and the
// checksum stripped (length reduced by 4, bit cleared).
func RewriteReplyHeader(hdr []byte, newID, responseTo int32) [20]byte {
	var out [20]byte
	copy(out[:], hdr[:20])
	length := int32(binary.LittleEndian.Uint32(out[0:]))
	flags := binary.LittleEndian.Uint32(out[16:])
	if flags&FlagChecksumPresent != 0 {
		length -= 4
		flags &^= FlagChecksumPresent
	}
	binary.LittleEndian.PutUint32(out[0:], uint32(length))
	binary.LittleEndian.PutUint32(out[4:], uint32(newID))
	binary.LittleEndian.PutUint32(out[8:], uint32(responseTo))
	binary.LittleEndian.PutUint32(out[16:], flags)
	return out
}

// Correlator hands out upstream request IDs that are unique among the
// outstanding requests of one upstream connection, and matches replies. It is
// not safe for concurrent use.
type Correlator struct {
	next int32
	out  map[int32]struct{}
}

// NewCorrelator returns an empty correlator.
func NewCorrelator() *Correlator { return &Correlator{out: map[int32]struct{}{}} }

// Next returns an ID not currently outstanding and records it. IDs are
// positive and never 0.
func (c *Correlator) Next() int32 {
	for {
		c.next++
		if c.next <= 0 {
			c.next = 1
		}
		if _, used := c.out[c.next]; !used {
			c.out[c.next] = struct{}{}
			return c.next
		}
	}
}

// Match completes the request whose ID is responseTo. An ID that is not
// outstanding is unexpected_response and changes nothing.
func (c *Correlator) Match(responseTo int32) error {
	if _, ok := c.out[responseTo]; !ok {
		return ErrUnexpectedResponse()
	}
	delete(c.out, responseTo)
	return nil
}

// Outstanding returns the number of outstanding requests.
func (c *Correlator) Outstanding() int { return len(c.out) }

// ClientIDs is the per-client counter of reply requestIDs (FR-11).
type ClientIDs struct{ next int32 }

// NewClientIDs returns a counter.
func NewClientIDs() *ClientIDs { return &ClientIDs{} }

// Next returns the next positive ID, wrapping before overflow.
func (c *ClientIDs) Next() int32 {
	c.next++
	if c.next <= 0 {
		c.next = 1
	}
	return c.next
}
