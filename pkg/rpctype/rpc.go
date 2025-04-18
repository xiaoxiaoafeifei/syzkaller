// Copyright 2017 syzkaller project authors. All rights reserved.
// Use of this source code is governed by Apache 2 LICENSE that can be found in the LICENSE file.

package rpctype

import (
	"compress/flate"
	"fmt"
	"io"
	"net"
	"net/rpc"
	"time"

	"github.com/google/syzkaller/pkg/log"
)

type RPCServer struct {
	ln net.Listener
	s  *rpc.Server
}

func NewRPCServer(addr, name string, receiver interface{}) (*RPCServer, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("failed to listen on %v: %w", addr, err)
	}
	s := rpc.NewServer()
	if err := s.RegisterName(name, receiver); err != nil {
		return nil, err
	}
	serv := &RPCServer{
		ln: ln,
		s:  s,
	}
	return serv, nil
}

func (serv *RPCServer) Serve() {
	for {
		conn, err := serv.ln.Accept()
		if err != nil {
			log.Logf(0, "failed to accept an rpc connection: %v", err)
			continue
		}
		setupKeepAlive(conn, time.Minute)
		go serv.s.ServeConn(newFlateConn(conn))
	}
}

func (serv *RPCServer) Addr() net.Addr {
	return serv.ln.Addr()
}

type RPCClient struct {
	conn net.Conn
	c    *rpc.Client
}

func NewRPCClient(addr string) (*RPCClient, error) {
	conn, err := net.DialTimeout("tcp", addr, 3*time.Minute)
	if err != nil {
		return nil, err
	}
	setupKeepAlive(conn, time.Minute)
	cli := &RPCClient{
		conn: conn,
		c:    rpc.NewClient(newFlateConn(conn)),
	}
	return cli, nil
}

func (cli *RPCClient) Call(method string, args, reply interface{}) error {
	// Note: SetDeadline is not implemented on fuchsia, so don't fail on error.
	cli.conn.SetDeadline(time.Now().Add(10 * time.Minute))
	defer cli.conn.SetDeadline(time.Time{})
	return cli.c.Call(method, args, reply)
}

func (cli *RPCClient) Close() {
	cli.c.Close()
}

func setupKeepAlive(conn net.Conn, keepAlive time.Duration) {
	conn.(*net.TCPConn).SetKeepAlive(true)
	conn.(*net.TCPConn).SetKeepAlivePeriod(keepAlive)
}

// flateConn wraps net.Conn in flate.Reader/Writer for compressed traffic.
type flateConn struct {
	r io.ReadCloser
	w *flate.Writer
	c io.Closer
}

func newFlateConn(conn io.ReadWriteCloser) io.ReadWriteCloser {
	w, err := flate.NewWriter(conn, 9)
	if err != nil {
		panic(err)
	}
	return &flateConn{
		r: flate.NewReader(conn),
		w: w,
		c: conn,
	}
}

func (fc *flateConn) Read(data []byte) (int, error) {
	return fc.r.Read(data)
}

func (fc *flateConn) Write(data []byte) (int, error) {
	n, err := fc.w.Write(data)
	if err != nil {
		return n, err
	}
	if err := fc.w.Flush(); err != nil {
		return n, err
	}
	return n, nil
}

func (fc *flateConn) Close() error {
	var err0 error
	if err := fc.r.Close(); err != nil {
		err0 = err
	}
	if err := fc.w.Close(); err != nil {
		err0 = err
	}
	if err := fc.c.Close(); err != nil {
		err0 = err
	}
	return err0
}
