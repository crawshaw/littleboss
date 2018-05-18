package lbrpc // import "crawshaw.io/littleboss/lbrpc"

import (
	"time"
)

type Request struct {
	// Type == "reload", "stop", or "info"
	Type string `json:"type"`

	// Type == "reload"
	Binary string   `json:"binary,omitempty"`
	Args   []string `json:"args,omitempty"`

	// Type == "stop" || Type == "reload"
	Timeout time.Duration `json:"timeout,omitempty"`
}

type ErrResponse struct {
	Error string `json:"error"`
}

type InfoResponse struct {
	Name      string    `json:"name"`
	PID       int       `json:"pid"`
	Start     time.Time `json:"start"`
	Binary    string    `json:"binary"`
	Args      []string  `json:"args"`
	BossPID   int       `json:"boss_pid"`
	BossStart time.Time `json:"boss_start"`
}

type StopResponse struct {
	Forced   bool `json:"forced,omitempty"` // timeout expired, process killed
	ExitCode int  `json:"exit_code"`
}

type ReloadResponse struct {
	StopResponse
}
