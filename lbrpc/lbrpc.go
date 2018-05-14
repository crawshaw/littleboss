package lbrpc // import "crawshaw.io/littleboss/lbrpc"

import (
	"time"
)

type Request struct {
	Type string `json:"type"`

	// Type == "start" || Type == "reload"
	Binary string   `json:"binary,omitempty"`
	Args   []string `json:"args,omitempty"`

	// Type == "stop" || Type == "reload"
	Timeout time.Duration `json:"timeout,omitempty"`
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

type StartResponse struct {
	ServicePID int `json:"service_pid"`
}

type StopResponse struct {
	Forced   bool `json:"forced,omitempty"` // timeout expired, process killed
	ExitCode int  `json:"exit_code"`
}

type ReloadResponse struct {
	StopResponse
}
