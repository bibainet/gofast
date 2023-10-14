// Copyright 2016 Yeung Shu Hung and The Go Authors.
// All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// This file implements the web server side for FastCGI
// as specified in http://www.mit.edu/~yandros/doc/specs/fcgi-spec.html

// A part of this file is from golang package net/http/cgi,
// in particular https://golang.org/src/net/http/cgi/host.go

package gofast

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"runtime"
	"strconv"
	"strings"
	"sync"
)

// Role for fastcgi application in spec
type Role uint16

// Roles specified in the fastcgi spec
const (
	RoleResponder Role = iota + 1
	RoleAuthorizer
	RoleFilter

	MaxRequestID = ^uint16(0)
)

// NewRequest returns a standard FastCGI request
// with a unique request ID allocted by the client
func NewRequest(r *http.Request) (req *Request) {
	req = &Request{
		Raw:    r,
		Role:   RoleResponder,
		Params: make(map[string]string),
	}

	// if no http request, return here
	if r == nil {
		return
	}

	// pass body (io.ReadCloser) to stdio
	req.Stdin = r.Body
	return
}

// Request hold information of a standard
// FastCGI request
type Request struct {
	Raw      *http.Request
	Role     Role
	Params   map[string]string
	Stdin    io.ReadCloser
	Data     io.ReadCloser
	KeepConn bool
}

type idPool struct {
	IDs uint16

	Used *sync.Map
	Lock *sync.Mutex
}

// AllocID implements Client.AllocID
func (p *idPool) Alloc() uint16 {
	p.Lock.Lock()
next:
	idx := p.IDs
	if idx == MaxRequestID {
		// reset
		p.IDs = 0
	}
	p.IDs++

	if _, inuse := p.Used.Load(idx); inuse {
		// Allow other go-routine to take priority
		// to prevent spinlock here
		runtime.Gosched()
		goto next
	}

	p.Used.Store(idx, struct{}{})
	p.Lock.Unlock()

	return idx
}

// ReleaseID implements Client.ReleaseID
func (p *idPool) Release(id uint16) {
	p.Used.Delete(id)
}

func newIDs() *idPool {
	return &idPool{
		Used: new(sync.Map),
		Lock: new(sync.Mutex),
		IDs:  uint16(1),
	}
}

// client is the default implementation of Client
type client struct {
	conn *conn
	ids  *idPool
}

// writeRequest writes params and stdin to the FastCGI application
func (c *client) writeRequest(reqID uint16, req *Request) (err error) {

	// end request whenever the function block ends
	defer func() {
		if err != nil {
			// abort the request if there is any error
			// in previous request writing process.
			c.conn.writeAbortRequest(reqID)
			return
		}
	}()

	// write request header with specified role
	err = c.conn.writeBeginRequest(reqID, req.Role, 1)
	if err != nil {
		return
	}
	err = c.conn.writePairs(typeParams, reqID, req.Params)
	if err != nil {
		return
	}

	// write the stdin stream
	stdinWriter := newWriter(c.conn, typeStdin, reqID)
	if req.Stdin != nil {
		defer req.Stdin.Close()
		p := make([]byte, 1024)
		var count int
		for {
			count, err = req.Stdin.Read(p)
			if err == io.EOF {
				err = nil
			} else if err != nil {
				stdinWriter.Close()
				return
			}
			if count == 0 {
				break
			}
			_, err = stdinWriter.Write(p[:count])
			if err != nil {
				stdinWriter.Close()
				return
			}
		}
	}
	if err = stdinWriter.Close(); err != nil {
		return
	}

	// for filter role, also add the data stream
	if req.Role == RoleFilter {
		// write the data stream
		dataWriter := newWriter(c.conn, typeData, reqID)
		defer req.Data.Close()
		p := make([]byte, 1024)
		var count int
		for {
			count, err = req.Data.Read(p)
			if err == io.EOF {
				err = nil
			} else if err != nil {
				return
			}
			if count == 0 {
				break
			}

			_, err = dataWriter.Write(p[:count])
			if err != nil {
				return
			}
		}
		if err = dataWriter.Close(); err != nil {
			return
		}
	}
	return
}

// readResponse read the FastCGI stdout and stderr, then write
// to the response pipe. Protocol error will also be written
// to the error writer in ResponsePipe.
func (c *client) readResponse(ctx context.Context, resp *ResponsePipe, req *Request) (err error) {

	var rec record
	done := make(chan int)

	// readloop in goroutine
	go func(rwc io.ReadWriteCloser) {
	readLoop:
		for {
			if err := rec.read(rwc); err != nil {
				break
			}

			// different output type for different stream
			switch rec.h.Type {
			case typeStdout:
				resp.stdOutWriter.Write(rec.content())
			case typeStderr:
				resp.stdErrWriter.Write(rec.content())
			case typeEndRequest:
				break readLoop
			default:
				err := fmt.Sprintf("unexpected type %#v in readLoop", rec.h.Type)
				resp.stdErrWriter.Write([]byte(err))
			}
		}
		close(done)
	}(c.conn.rwc)

	select {
	case <-ctx.Done():
		// do nothing, let client.Do handle
		err = fmt.Errorf("timeout or canceled")
	case <-done:
		// do nothing and end the function
	}
	return
}

// Do implements Client.Do
func (c *client) Do(req *Request) (resp *ResponsePipe, err error) {

	// validate the request
	// if role is a filter, it has to have Data stream
	if req.Role == RoleFilter {
		// validate the request
		if req.Data == nil {
			err = fmt.Errorf("filter request requires a data stream")
		} else if _, ok := req.Params["FCGI_DATA_LAST_MOD"]; !ok {
			err = fmt.Errorf("filter request requires param FCGI_DATA_LAST_MOD")
		} else if _, err = strconv.ParseUint(req.Params["FCGI_DATA_LAST_MOD"], 10, 32); err != nil {
			err = fmt.Errorf("invalid parsing FCGI_DATA_LAST_MOD (%s)", err)
		} else if _, ok := req.Params["FCGI_DATA_LENGTH"]; !ok {
			err = fmt.Errorf("filter request requires param FCGI_DATA_LENGTH")
		} else if _, err = strconv.ParseUint(req.Params["FCGI_DATA_LENGTH"], 10, 32); err != nil {
			err = fmt.Errorf("invalid parsing FCGI_DATA_LENGTH (%s)", err)
		}

		// if invalid, end the response stream and return
		if err != nil {
			return
		}
	}

	// check if connection exists
	if c.conn == nil {
		err = fmt.Errorf("client connection has been closed")
		return
	}

	// allocate request ID
	reqID := c.ids.Alloc()

	// create response pipe
	resp = NewResponsePipe()
	rwError, allDone := make(chan error), make(chan int)

	// if there is a raw request, use the context deadline
	var ctx context.Context
	if req.Raw != nil {
		ctx = req.Raw.Context()
	} else {
		ctx = context.TODO()
	}

	// wait group to wait for both read and write to end
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		wg.Wait()
		close(allDone)
	}()

	// Run read and write in parallel.
	// Note: Specification never said "write before read".

	// write the request through request pipe
	go func() {
		if err := c.writeRequest(reqID, req); err != nil {
			rwError <- err
		}
		wg.Done()
	}()

	// get response from client and write through response pipe
	go func() {
		if err := c.readResponse(ctx, resp, req); err != nil {
			rwError <- err
		}
		wg.Done()
	}()

	// do not block the return of client.Do
	// and return the response pipes
	// (or else would be block by the response pipes not being used)
	go func() {
		// wait until context deadline
		// or until writeError is not blocked.
	loop:
		for {
			select {
			case err := <-rwError:
				// pass the read / write error to error stream
				resp.stdErrWriter.Write([]byte(err.Error()))
				continue
			case <-allDone:
				break loop
				// do nothing
			}
		}

		// clean up
		c.ids.Release(reqID)
		resp.Close()
		close(rwError)
	}()
	return
}

// Close implements Client.Close
// If the inner connection has been closed before,
// this method would do nothing and return nil
func (c *client) Close() (err error) {
	if c.conn == nil {
		return
	}
	err = c.conn.Close()
	c.conn = nil
	return
}

// Client is a client interface of FastCGI
// application process through given
// connection (net.Conn)
type Client interface {

	// Do  a proper FastCGI request.
	// Returns the response streams (stdout and stderr)
	// and the request validation error.
	//
	// Note: protocol error will be written to the stderr
	// stream in the ResponsePipe.
	Do(req *Request) (resp *ResponsePipe, err error)

	// Close the underlying connection
	Close() error
}

// ConnFactory creates new network connections
// to the FPM application
type ConnFactory func() (net.Conn, error)

// SimpleConnFactory creates the simplest ConnFactory implementation.
func SimpleConnFactory(network, address string) ConnFactory {
	return func() (net.Conn, error) {
		return net.Dial(network, address)
	}
}

// ClientFactory creates new FPM client with proper connection
// to the FPM application.
type ClientFactory func() (Client, error)

// SimpleClientFactory returns a ClientFactory implementation
// with the given ConnFactory.
func SimpleClientFactory(connFactory ConnFactory) ClientFactory {
	return func() (c Client, err error) {
		// connect to given network address
		conn, err := connFactory()
		if err != nil {
			return
		}
		// create client
		c = &client{
			conn: newConn(conn),
			ids:  newIDs(),
		}
		return
	}
}

// NewResponsePipe returns an initialized new ResponsePipe struct
func NewResponsePipe() (p *ResponsePipe) {
	p = new(ResponsePipe)
	p.stdOutReader, p.stdOutWriter = io.Pipe()
	p.stdErrReader, p.stdErrWriter = io.Pipe()
	return
}

// ResponsePipe contains readers and writers that handles
// all FastCGI output streams
type ResponsePipe struct {
	stdOutReader io.Reader
	stdOutWriter io.WriteCloser
	stdErrReader io.Reader
	stdErrWriter io.WriteCloser
}

// Close close all writers
func (pipes *ResponsePipe) Close() {
	pipes.stdOutWriter.Close()
	pipes.stdErrWriter.Close()
}

// WriteTo writes backend's STDOUT to http.ResponseWriter, STDERR to ew
func (pipes *ResponsePipe) WriteTo(w http.ResponseWriter, ew io.Writer) error {
	var ce = make(chan error, 1)
	go func() {
		ce <- pipes.writeResponse(w)
	}()
	go pipes.writeError(ew) // Ignore I/O errors on STDERR stream
	var e = <-ce
	close(ce)
	return e
	/*
	var ce = make(chan error, 2)
	go func() {
		ce <- pipes.writeResponse(w)
	}()
	go func() {
		ce <- pipes.writeError(ew)
	}()
	var e = <-ce
	if e == nil {
		e = <-ce
	} else {
		<-ce
	}
	close(ce)
	return e
	// */
}

// Write backend's STDERR to ew, return I/O error
func (pipes *ResponsePipe) writeError(ew io.Writer) error {
	var _, e = io.Copy(ew, pipes.stdErrReader)
	return e
}

// Write backend's STDOUT to http.ResponseWriter, parse headers, return I/O or parsing error
func (pipes *ResponsePipe) writeResponse(w http.ResponseWriter) error {
	var body = bufio.NewReaderSize(pipes.stdOutReader, 1024)
	var headers = make(http.Header)
	var header, value string
	var statusCode, headerLines int
	var hasBlankLine, cut bool
	var line []byte
	var e error
	// Read headers
	for {
		line, cut, e = body.ReadLine()
		if cut {
			w.WriteHeader(http.StatusInternalServerError)
			return fmt.Errorf("long header line from subprocess")
		}
		if e != nil {
			if e == io.EOF {
				break
			}
			w.WriteHeader(http.StatusInternalServerError)
			return fmt.Errorf("error reading headers: %v", e)
		}
		if len(line) == 0 {
			hasBlankLine = true
			break
		}
		headerLines++
		header, value, cut = strings.Cut(string(line), ":")
		header, value = strings.TrimSpace(header), strings.TrimSpace(value)
		if !cut || header == "" {
			return fmt.Errorf("invalid header line: %s", line)
		}
		switch header {
		case "Status":
			statusCode = 0
			if len(value) >= 3 {
				statusCode, _ = strconv.Atoi(value[:3])
			}
			if statusCode == 0 {
				return fmt.Errorf("invalid status: %q", value)
			}
		default:
			headers.Add(header, value)
		}
	}
	// Check header reading results
	if headerLines == 0 || !hasBlankLine {
		w.WriteHeader(http.StatusInternalServerError)
		return fmt.Errorf("no headers")
	}
	if statusCode == 0 {
		if headers.Get("Location") != "" {
			statusCode = http.StatusFound
		} else {
			statusCode = http.StatusOK
		}
	}
	// Copy headers to rw's headers, after we've decided not to
	// go into handleInternalRedirect, which won't want its rw
	// headers to have been touched.
	var whdr = w.Header()
	for k, vv := range headers {
		whdr[k] = vv
	}
	w.WriteHeader(statusCode)
	// Send remaining body
	_, e = io.Copy(w, body) // It calls w.ReadFrom(body) which uses a buffers pool
	return e
}

// ClientFunc is a function wrapper of a Client interface shortcut implementation.
// Mainly for testing and development purpose.
type ClientFunc func(req *Request) (resp *ResponsePipe, err error)

// Do implements Client.Do
func (c ClientFunc) Do(req *Request) (resp *ResponsePipe, err error) {
	return c(req)
}

// Close implements Client.Close
func (c ClientFunc) Close() error {
	return nil
}
