// Package fcgi is a minimal FastCGI responder client for tests (BEGIN_REQUEST,
// PARAMS, STDIN; reads STDOUT, STDERR and END_REQUEST). Each Do dials a new
// connection. Test infrastructure only.
package fcgi

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"strconv"
	"strings"
	"time"
)

const (
	typeBegin     = 1
	typeEnd       = 3
	typeParams    = 4
	typeStdin     = 5
	typeStdout    = 6
	typeStderr    = 7
	maxRecord     = 65535
	requestID     = 1
	roleResponder = 1
)

// Request is one responder request.
type Request struct {
	Params map[string]string
	Stdin  []byte
}

// Response is the collected reply.
type Response struct {
	Stdout, Stderr []byte
	AppStatus      uint32
	ProtocolStatus byte
}

func headerEnd(b []byte) (idx, sep int) {
	if i := bytes.Index(b, []byte("\r\n\r\n")); i >= 0 {
		return i, 4
	}
	if i := bytes.Index(b, []byte("\n\n")); i >= 0 {
		return i, 2
	}
	return -1, 0
}

// Body returns stdout after the CGI header block.
func (r *Response) Body() []byte {
	i, n := headerEnd(r.Stdout)
	if i < 0 {
		return r.Stdout
	}
	return r.Stdout[i+n:]
}

// StatusCode returns the code from a "Status:" header, default 200.
func (r *Response) StatusCode() int {
	i, _ := headerEnd(r.Stdout)
	if i < 0 {
		return 200
	}
	for _, line := range strings.Split(string(r.Stdout[:i]), "\n") {
		line = strings.TrimSpace(line)
		if len(line) > 7 && strings.EqualFold(line[:7], "status:") {
			f := strings.Fields(line[7:])
			if len(f) > 0 {
				if n, err := strconv.Atoi(f[0]); err == nil {
					return n
				}
			}
		}
	}
	return 200
}

func record(typ byte, payload []byte) []byte {
	h := []byte{1, typ, 0, requestID, byte(len(payload) >> 8), byte(len(payload)), 0, 0}
	return append(h, payload...)
}

func stream(typ byte, data []byte) []byte {
	var out []byte
	for len(data) > 0 {
		n := min(len(data), maxRecord)
		out = append(out, record(typ, data[:n])...)
		data = data[n:]
	}
	return append(out, record(typ, nil)...)
}

func encLen(b []byte, n int) []byte {
	if n < 128 {
		return append(b, byte(n))
	}
	return binary.BigEndian.AppendUint32(b, uint32(n)|1<<31)
}

// Do sends req to addr and reads the response; timeout bounds the whole call.
func Do(addr string, req Request, timeout time.Duration) (*Response, error) {
	c, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(timeout))

	var params []byte
	for k, v := range req.Params {
		params = encLen(params, len(k))
		params = encLen(params, len(v))
		params = append(params, k...)
		params = append(params, v...)
	}
	out := record(typeBegin, []byte{0, roleResponder, 0, 0, 0, 0, 0, 0})
	out = append(out, stream(typeParams, params)...)
	out = append(out, stream(typeStdin, req.Stdin)...)
	if _, err := c.Write(out); err != nil {
		return nil, err
	}

	resp := &Response{}
	for {
		var h [8]byte
		if _, err := io.ReadFull(c, h[:]); err != nil {
			return nil, errors.New("fcgi: connection ended before END_REQUEST")
		}
		n := int(binary.BigEndian.Uint16(h[4:]))
		pad := int(h[6])
		buf := make([]byte, n+pad)
		if _, err := io.ReadFull(c, buf); err != nil {
			return nil, errors.New("fcgi: truncated record")
		}
		buf = buf[:n]
		switch h[1] {
		case typeStdout:
			resp.Stdout = append(resp.Stdout, buf...)
		case typeStderr:
			resp.Stderr = append(resp.Stderr, buf...)
		case typeEnd:
			if len(buf) < 5 {
				return nil, errors.New("fcgi: short END_REQUEST")
			}
			resp.AppStatus = binary.BigEndian.Uint32(buf)
			resp.ProtocolStatus = buf[4]
			return resp, nil
		}
	}
}
