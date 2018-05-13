package main

import (
	"bufio"
	"bytes"
	"fmt"
	"log"
	"os/exec"
	"regexp"
	"strconv"
)

func FindDaemons(cmdpath string) (daemons []DaemonInfo, err error) {
	cmd := exec.Command("ps", "-x", "-o", "pid command")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("FindDaemons: ps failed: %v (%s)", err, out)
	}
	return parsePS(cmdpath, out)
}

type DaemonInfo struct {
	PID         int
	ServiceName string
	SocketPath  string
}

func parsePS(cmdpath string, b []byte) (daemons []DaemonInfo, err error) {
	scanner := bufio.NewScanner(bytes.NewReader(b))
	for scanner.Scan() {
		if dinfo := parsePSLine(cmdpath, scanner.Bytes()); dinfo != nil {
			daemons = append(daemons, *dinfo)
		}
	}
	if scanner.Err() != nil {
		return nil, fmt.Errorf("psinfo: cannot parse ps output: %v", err)
	}
	return daemons, nil
}

var daemonArgsRE = regexp.MustCompile(`^-daemon -name=([A-Za-z0-9\-_]*) -socketpath=([A-Za-z0-9/.\-_]*)`)

func parsePSLine(cmdpath string, b []byte) *DaemonInfo {
	dinfo := &DaemonInfo{}

	if i := bytes.IndexByte(b, ' '); i < 1 {
		return nil
	} else {
		pid, err := strconv.Atoi(string(b[:i]))
		if err != nil {
			return nil
		}
		dinfo.PID = int(pid)
		b = b[i+1:]
	}

	if len(b) <= len(cmdpath) || string(b[:len(cmdpath)]) != cmdpath {
		return nil // typical case, this is not an ltboss daemon
	}
	b = b[len(cmdpath)+1:]
	m := daemonArgsRE.FindSubmatch(b)
	if m == nil {
		log.Printf("parsePSLine: no match %q", b)
		return nil
	}
	dinfo.ServiceName = string(m[1])
	dinfo.SocketPath = string(m[2])
	return dinfo
}
