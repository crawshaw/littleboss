package rpc

import (
	"encoding/json"
	"fmt"
	"net"
	"time"
)

type Request struct {
	Type string `json:"type"`
}

type ErrResponse struct {
	Error string `json:"error"`
}

type InfoResponse struct {
	ServiceName  string    `json:"service_name"`
	ServicePID   int       `json:"service_pid"`
	ServiceStart time.Time `json:"service_start"`
	BossPID      int       `json:"boss_pid"`
	BossStart    time.Time `json:"boss_start"`
}

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

func (c *Client) Info() (*InfoResponse, error) {
	c.conn.SetDeadline(time.Now().Add(1 * time.Second))
	if err := c.w.Encode(Request{Type: "info"}); err != nil {
		return nil, fmt.Errorf("info: %v", err)
	}
	res := new(InfoResponse)
	if err := c.r.Decode(res); err != nil {
		return nil, fmt.Errorf("info: %v", err)
	}
	return res, nil
}
