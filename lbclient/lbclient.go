package lbclient

import (
	"encoding/json"
	"fmt"
	"net"
	"time"

	"crawshaw.io/littleboss/lbrpc"
)

type Client struct {
	SocketPath string

	conn net.Conn
	w    *json.Encoder
	r    *json.Decoder
}

func NewClient(socketpath string) (*Client, error) {
	conn, err := net.DialTimeout("unix", socketpath, 500*time.Millisecond)
	if err != nil {
		return nil, err
	}

	return &Client{
		SocketPath: socketpath,
		conn:       conn,
		w:          json.NewEncoder(conn),
		r:          json.NewDecoder(conn),
	}, nil
}

func (c *Client) Info() (*lbrpc.InfoResponse, error) {
	c.conn.SetDeadline(time.Now().Add(1 * time.Second))
	if err := c.w.Encode(lbrpc.Request{Type: "info"}); err != nil {
		return nil, fmt.Errorf("info: %v", err)
	}
	res := new(lbrpc.InfoResponse)
	if err := c.r.Decode(res); err != nil {
		return nil, fmt.Errorf("info: %v", err)
	}
	return res, nil
}

func (c *Client) Start(binpath string, args []string) (*lbrpc.StartResponse, error) {
	c.conn.SetDeadline(time.Now().Add(1 * time.Second))
	req := lbrpc.Request{
		Type:   "start",
		Binary: binpath,
		Args:   args,
	}
	if err := c.w.Encode(req); err != nil {
		return nil, fmt.Errorf("start: %v", err)
	}
	res := new(lbrpc.StartResponse)
	if err := c.r.Decode(res); err != nil {
		return nil, fmt.Errorf("start: %v", err)
	}
	return res, nil
}
