// Copyright 2011 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package gofast implements the FastCGI protocol.
// Currently only the responder role is supported.
// The protocol is defined at http://www.mit.edu/~yandros/doc/specs/fcgi-spec.html
package gofast

// This file defines the raw protocol and some utilities used by the child and
// the host.

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"sync"
)

// recType is a record type, as defined by
// http://www.fastcgi.com/devkit/doc/fcgi-spec.html#S8
type recType uint8

const (
	typeBeginRequest    recType = 1
	typeAbortRequest    recType = 2
	typeEndRequest      recType = 3
	typeParams          recType = 4
	typeStdin           recType = 5
	typeStdout          recType = 6
	typeStderr          recType = 7
	typeData            recType = 8
	typeGetValues       recType = 9
	typeGetValuesResult recType = 10
	typeUnknownType     recType = 11
)

// String implements fmt.Stringer
func (t recType) String() string {
	switch t {
	case typeBeginRequest:
		return "FCGI_BEGIN_REQUEST"
	case typeAbortRequest:
		return "FCGI_BEGIN_REQUEST"
	case typeEndRequest:
		return "FCGI_END_REQUEST"
	case typeParams:
		return "FCGI_PARAMS"
	case typeStdin:
		return "FCGI_STDIN"
	case typeStdout:
		return "FCGI_STDOUT"
	case typeStderr:
		return "FCGI_STDERR"
	case typeData:
		return "FCGI_DATA"
	case typeGetValues:
		return "FCGI_GET_VALUES"
	case typeGetValuesResult:
		return "FCGI_GET_VALUES_RESULT"
	case typeUnknownType:
		fallthrough
	default:
		return "FCGI_UNKNOWN_TYPE"
	}
}

// GoString implements fmt.GoStringer
func (t recType) GoString() string {
	return t.String()
}

// keep the connection between web-server and responder open after request
const flagKeepConn = 1

const (
	maxWrite = 65535 // maximum record body
	maxPad   = 255
)

const (
	roleResponder = iota + 1 // only Responders are implemented.
	roleAuthorizer
	roleFilter
)

const (
	statusRequestComplete = iota
	statusCantMultiplex
	statusOverloaded
	statusUnknownRole
)

const headerLen = 8

type header struct {
	Version       uint8
	Type          recType
	ID            uint16
	ContentLength uint16
	PaddingLength uint8
	Reserved      uint8
}

type beginRequest struct {
	role     uint16
	flags    uint8
	reserved [5]uint8
}

func (br *beginRequest) read(content []byte) error {
	if len(content) != 8 {
		return errors.New("fcgi: invalid begin request record")
	}
	br.role = binary.BigEndian.Uint16(content)
	br.flags = content[2]
	return nil
}

// for padding so we don't have to allocate all the time
// not synchronized because we don't care what the contents are
var pad [maxPad]byte

func (h *header) init(recType recType, reqID uint16, contentLength int) {
	h.Version = 1
	h.Type = recType
	h.ID = reqID
	h.ContentLength = uint16(contentLength)
	h.PaddingLength = uint8(-contentLength & 7)
}

// conn sends records over rwc
type conn struct {
	mutex sync.Mutex
	rwc   io.ReadWriteCloser

	// to avoid allocations
	buf bytes.Buffer
	h   header
}

func newConn(rwc io.ReadWriteCloser) *conn {
	return &conn{rwc: rwc}
}

func (c *conn) Close() error {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	return c.rwc.Close()
}

type record struct {
	h   header
	buf [maxWrite + maxPad]byte
}

func (rec *record) read(r io.Reader) (err error) {
	if err = binary.Read(r, binary.BigEndian, &rec.h); err != nil {
		return err
	}
	if rec.h.Version != 1 {
		return errors.New("fcgi: invalid header version")
	}
	n := int(rec.h.ContentLength) + int(rec.h.PaddingLength)
	if _, err = io.ReadFull(r, rec.buf[:n]); err != nil {
		return err
	}
	return nil
}

func (rec *record) content() []byte {
	return rec.buf[:rec.h.ContentLength]
}

// writeRecord writes and sends a single record.
func (c *conn) writeRecord(recType recType, reqID uint16, b []byte) error {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	c.buf.Reset()
	c.h.init(recType, reqID, len(b))
	if err := binary.Write(&c.buf, binary.BigEndian, c.h); err != nil {
		return err
	}
	if _, err := c.buf.Write(b); err != nil {
		return err
	}
	if _, err := c.buf.Write(pad[:c.h.PaddingLength]); err != nil {
		return err
	}
	_, err := c.rwc.Write(c.buf.Bytes())
	return err
}

func (c *conn) writeBeginRequest(reqID uint16, role Role, flags uint8) error {
	b := [8]byte{byte(role >> 8), byte(role), flags}
	return c.writeRecord(typeBeginRequest, reqID, b[:])
}

func (c *conn) writeEndRequest(reqID uint16, appStatus int, protocolStatus uint8) error {
	b := make([]byte, 8)
	binary.BigEndian.PutUint32(b, uint32(appStatus))
	b[4] = protocolStatus
	return c.writeRecord(typeEndRequest, reqID, b)
}

func (c *conn) writeAbortRequest(reqID uint16) error {
	return c.writeRecord(typeAbortRequest, reqID, nil)
}

func (c *conn) writePairs(recType recType, reqID uint16, pairs map[string]string) error {
	// keys := make([]string, 0, len(pairs))
	// for k := range pairs {
		// keys = append(keys, k)
	// }
	// sort.Strings(keys)
	var b [8]byte
	var n int
	var e error
	var w = newWriter(c, recType, reqID)
	for k, v := range pairs {
		n =  encodeSize(b[0:], uint32(len(k)))
		n += encodeSize(b[n:], uint32(len(v)))
		if _, e = w.Write(b[:n]); e != nil {
			return e
		}
		if _, e = w.WriteString(k); e != nil {
			return e
		}
		if _, e = w.WriteString(v); e != nil {
			return e
		}
	}
	return w.Close()
}

func readSize(s []byte) (uint32, int) {
	if len(s) == 0 {
		return 0, 0
	}
	size, n := uint32(s[0]), 1
	if size&(1<<7) != 0 {
		if len(s) < 4 {
			return 0, 0
		}
		n = 4
		size = binary.BigEndian.Uint32(s)
		size &^= 1 << 31
	}
	return size, n
}

func readString(s []byte, size uint32) string {
	if size > uint32(len(s)) {
		return ""
	}
	return string(s[:size])
}

func encodeSize(b []byte, size uint32) int {
	if size > 127 {
		size |= 1 << 31
		binary.BigEndian.PutUint32(b, size)
		return 4
	}
	b[0] = byte(size)
	return 1
}

// bufWriter encapsulates bufio.Writer but also closes the underlying stream when
// Closed.
type bufWriter struct {
	closer io.Closer
	*bufio.Writer
}

func (w *bufWriter) Close() error {
	if err := w.Writer.Flush(); err != nil {
		w.closer.Close()
		return err
	}
	return w.closer.Close()
}

func newWriter(c *conn, recType recType, reqID uint16) *bufWriter {
	s := &streamWriter{c: c, recType: recType, reqID: reqID}
	w := bufio.NewWriterSize(s, maxWrite)
	return &bufWriter{s, w}
}

// streamWriter abstracts out the separation of a stream into discrete records.
// It only writes maxWrite bytes at a time.
type streamWriter struct {
	c       *conn
	recType recType
	reqID   uint16
}

func (w *streamWriter) Write(p []byte) (int, error) {
	nn := 0
	for len(p) > 0 {
		n := len(p)
		if n > maxWrite {
			n = maxWrite
		}
		if err := w.c.writeRecord(w.recType, w.reqID, p[:n]); err != nil {
			return nn, err
		}
		nn += n
		p = p[n:]
	}
	return nn, nil
}

func (w *streamWriter) Close() error {
	// send empty record to close the stream
	return w.c.writeRecord(w.recType, w.reqID, nil)
}
