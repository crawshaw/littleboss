// Copyright (c) 2018 David Crawshaw <david@zentus.com>
//
// Permission to use, copy, modify, and distribute this software for any
// purpose with or without fee is hereby granted, provided that the above
// copyright notice and this permission notice appear in all copies.
//
// THE SOFTWARE IS PROVIDED "AS IS" AND THE AUTHOR DISCLAIMS ALL WARRANTIES
// WITH REGARD TO THIS SOFTWARE INCLUDING ALL IMPLIED WARRANTIES OF
// MERCHANTABILITY AND FITNESS. IN NO EVENT SHALL THE AUTHOR BE LIABLE FOR
// ANY SPECIAL, DIRECT, INDIRECT, OR CONSEQUENTIAL DAMAGES OR ANY DAMAGES
// WHATSOEVER RESULTING FROM LOSS OF USE, DATA OR PROFITS, WHETHER IN AN
// ACTION OF CONTRACT, NEGLIGENCE OR OTHER TORTIOUS ACTION, ARISING OUT OF
// OR IN CONNECTION WITH THE USE OR PERFORMANCE OF THIS SOFTWARE.

// Package littleboss creates self-supervising Go binaries.
//
// A self-supervising binary starts itself as a child process.
// The parent process becomes a supervisor that is responsible for
// monitoring, managing I/O, restarting, and reloading the child.
//
// Make a program use littleboss by modifying the main function:
//
//	func main() {
//		lb := littleboss.New("service-name")
//		lb.Run(func(ctx context.Context) {
//			// main goes here, exit when <-ctx.Done()
//		})
//	}
//
//
// Usage
//
// By default the supervisor is bypassed and the program executes directly.
// A flag, -littleboss, is added to the binary.
// It can be used to start a supervised binary and manage it:
//
//	$ mybin &                     # binary runs directly, no child process
//	$ mybin -littleboss=start &   # supervisor is created
//	$ mybin2 -littleboss=reload   # child is replaced by new mybin2 process
//	$ mybin -littleboss=stop      # supervisor and child are shut down
//
//
// Configuration
//
// Supervisor options are baked into the binary.
// The Littleboss struct type contains fields that can be set before calling
// the Run method to configure the supervisor.
package littleboss

// TODO version both protocols
// TODO document the parent<->child pipe protocol
// TODO reload changing flags
// TODO instrument supervisor
// TODO bring child up before shutting down old binary

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"os/user"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

type Littleboss struct {
	FlagSet *flag.FlagSet // used for passing flags to child, default flag.CommandLine

	Logf func(format string, args ...interface{}) // defaults to stderr

	SupervisorInit func() // executed by supervisor on Run

	Persist           bool          // if program exits, restart it
	FallbackOnFailure bool          // if program exits badly, use previous version
	LameduckTimeout   time.Duration // time to wait before forcing exit

	// Fields set once in New.
	name       string
	cmdname    string
	username   string
	reload     chan string
	reloadDone chan error

	// Fields fixed by Run.
	modeFlagName string
	mode         *string
	running      bool
	lnFlags      map[string]*ListenerFlag

	// Fields used by runChild and "status"/"stop"/"reload" handlers.
	mu           sync.Mutex
	childProcess *os.Process
	childPiper   *piper
	status       status
	stopSignal   chan int // exitCode
}

const usage = `instruct the littleboss:

start:		start and manage this process using service name %q
stop:		signal the littleboss to shutdown the process
status:		print statistics about the running littleboss
reload:		restart the managed process using the executed binary
bypass:		disable littleboss, run the program directly`

// A ListenerFlag is a flag whose value is used as a net.Listener or net.PacketConn.
//
// An empty flag value ("") means no listener is created.
// For a listener on a random port, use ":0".
type ListenerFlag struct {
	lb  *Littleboss
	net string
	val string
	ln  net.Listener
	pc  net.PacketConn
	f   *os.File // listener fd in children
}

func (lnf *ListenerFlag) String() string {
	if lnf.lb != nil && lnf.lb.mode != nil && *lnf.lb.mode == "child" {
		if i := strings.LastIndex(lnf.val, ":fd:"); i >= 0 {
			return lnf.val[:i]
		}
	}
	return lnf.val
}

func (lnf *ListenerFlag) Network() string            { return lnf.net }
func (lnf *ListenerFlag) Listener() net.Listener     { return lnf.ln }
func (lnf *ListenerFlag) PacketConn() net.PacketConn { return lnf.pc }
func (lnf *ListenerFlag) Set(value string) error {
	lnf.val = value
	return nil
}

func New(serviceName string) *Littleboss {
	if !isValidServiceName(serviceName) {
		panic(fmt.Sprintf("littleboss: invalid service name %q, must be [a-zA-Z0-9_-]", serviceName))
	}
	uid := os.Geteuid()
	u, err := user.LookupId(strconv.Itoa(uid))
	if err != nil {
		panic(fmt.Sprintf("littleboss: cannot determine user name: %v", err))
	}

	lb := &Littleboss{
		FlagSet: flag.CommandLine,

		FallbackOnFailure: true,
		LameduckTimeout:   2 * time.Second,

		name:       serviceName,
		cmdname:    os.Args[0],
		username:   u.Username,
		reload:     make(chan string, 1),
		reloadDone: make(chan error),
		lnFlags:    make(map[string]*ListenerFlag),
	}
	lb.Logf = func(format string, args ...interface{}) {
		fmt.Fprintf(lb.stderr(), "littleboss: %s\n", fmt.Sprintf(format, args...))
	}
	return lb
}

func (lb *Littleboss) Listener(flagName, network, value, usage string) *ListenerFlag {
	if lb.running {
		panic("littleboss: cannot create Listener flag after calling Run")
	}
	lnf := &ListenerFlag{
		lb:  lb,
		val: value,
		net: network,
	}
	lb.lnFlags[flagName] = lnf
	lb.FlagSet.Var(lnf, flagName, usage)
	return lnf
}

// Command gives littleboss a custom command mode flag name and value.
//
// By default littleboss creates a flag named -littleboss when
// Run is called, and sets its default value to "bypass".
// If the FlagSet is going to be processed by a
// non-standard flag package or the littleboss command mode
// is not passed as a flag at all, then this Command method can
// be used instead to avoid the flag creation.
//
// This makes it possible for a program to invoke flag parsing
// itself, after calling New, Command, any calls to Listener,
// but before the call to Run.
//
// For example:
//
//	lb := littleboss.New("myservice")
//	lb.Command("-mylb", pflag.String("mylb", "start", "lb command mode"))
//	pflag.CommandLine.AddGoFlagSet(flag.CommandLine)
//	pflag.Parse()
//	lb.Run(...)
//
// NOTE: All the flags passed to the child process are
// extracted from Littleboss.FlagSet. Make sure any flags defined
// by external flag packages have their value in the FlagSet.
func (lb *Littleboss) Command(modeFlagName string, mode *string) {
	lb.modeFlagName = modeFlagName
	lb.mode = mode
}

// Run starts the little boss, with mainFn as the entry point to the service.
// Run does not return.
func (lb *Littleboss) Run(mainFn func(ctx context.Context)) {
	if lb.running {
		panic("littleboss: Run called multiple times")
	}

	if lb.mode == nil {
		if lb.FlagSet.Parsed() {
			panic(fmt.Sprintf("littleboss: flags parsed before Run, but Command method not invoked"))
		}
		lb.modeFlagName = "littleboss"
		lb.mode = lb.FlagSet.String(lb.modeFlagName, "bypass", fmt.Sprintf(usage, lb.name))
	}
	if !lb.FlagSet.Parsed() {
		lb.FlagSet.Parse(os.Args[1:])
	}

	if lb.Logf == nil {
		lb.Logf = func(format string, args ...interface{}) {} // do nothing
	}

	switch *lb.mode {
	case "stop":
		lb.issueStop()
	case "reload":
		lb.issueReload()
	case "status":
		lb.issueStatus()
	case "child":
		lb.child(mainFn)
	}

	for name, lnf := range lb.lnFlags {
		if lnf.val == "" {
			continue
		}
		var ln net.Listener
		var pc net.PacketConn
		var err error
		if strings.HasPrefix(lnf.val, "fd:") {
			fd, err := strconv.Atoi(lnf.val[7:])
			if err != nil {
				fmt.Fprintf(lb.stderr(), "%s: -%s: invalid fd number: %v\n", lb.cmdname, name, err)
				os.Exit(1)
			}
			f := os.NewFile(uintptr(fd), name)
			if strings.HasPrefix(lnf.val, "fd:udp:") {
				pc, err = net.FilePacketConn(f)
			} else if strings.HasPrefix(lnf.val, "fd:tcp:") {
				ln, err = net.FileListener(f)
			} else {
				fmt.Fprintf(lb.stderr(), "%s: -%s: unknown fd type: %s\n", lb.cmdname, name, lnf.val)
				os.Exit(1)
			}
		} else if strings.HasPrefix(lnf.net, "tcp") {
			ln, err = net.Listen(lnf.net, lnf.val)
		} else {
			pc, err = net.ListenPacket(lnf.net, lnf.val)
		}
		if err != nil {
			fmt.Fprintf(lb.stderr(), "%s: -%s: %v\n", lb.cmdname, name, err)
			os.Exit(1)
		}
		if *lb.mode == "start" {
			type fileSource interface {
				File() (*os.File, error)
			}
			if src, _ := ln.(fileSource); ln != nil {
				f, err := src.File()
				if err != nil {
					lb.fatalf("could not get TCP listener fd: %v", err)
				}
				lnf.f = f
			} else if src, _ := pc.(fileSource); pc != nil {
				f, err := src.File()
				if err != nil {
					lb.fatalf("could not get packet conn fd: %v", err)
				}
				lnf.f = f
			} else {
				lb.fatalf("unsupported listener type: %T/%T", ln, pc)
			}
		}
		lnf.ln = ln
		lnf.pc = pc
	}

	lb.running = true

	switch *lb.mode {
	case "bypass":
		// Littleboss is not in use, run the program as if we weren't here.
		mainFn(context.Background())
		os.Exit(0)
	case "start":
		lb.startBoss()
	default:
		fmt.Fprintf(lb.stderr(), "%s: unknown littleboss command: %q\n\n", lb.cmdname, *lb.mode)
		lb.FlagSet.Usage()
		os.Exit(2)
	}

	panic("unexpected exit")
}

func (lb *Littleboss) startBoss() {
	socketdir := lb.socketdir()
	os.Mkdir(socketdir, 0700)
	os.Chown(socketdir, os.Geteuid(), os.Getegid())
	os.Chmod(socketdir, 0700)
	fi, err := os.Stat(socketdir)
	if err != nil {
		lb.fatalf("cannot stat socket dir %v", err)
	}
	if !fi.IsDir() {
		lb.fatalf("socket directory %s is not a directory", socketdir)
	}
	if fi.Mode().Perm() != 0700 {
		lb.fatalf("socket directory has bad permissions, want 0700 got %v", fi.Mode().Perm())
	}

	socketpath := filepath.Join(socketdir, lb.name+".socket")
	addr, err := net.ResolveUnixAddr("unix", socketpath) // "unix" == SOCK_STREAM
	if err != nil {
		lb.fatalf("resolve %v", err)
	}
	ln, err := net.ListenUnix("unix", addr)
	if err != nil {
		lb.fatalf("listen %v", err)
	}
	if f, _ := ln.File(); f != nil {
		f.Chmod(0700) // TODO test this, or remove this?
	}

	childPath, err := os.Executable()
	if err != nil {
		lb.fatalf("cannot find oneself: %v", err)
	}

	if lb.SupervisorInit != nil {
		lb.SupervisorInit()
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigCh
		fmt.Fprintf(lb.stderr(), "littleboss: signal received: %s\n", sig)
		stopSignal := make(chan int)
		lb.handleStopBegin(stopSignal)

		var exitCode int
		stopped := false
		select {
		case exitCode = <-stopSignal:
			stopped = true
		case <-time.After(100 * time.Millisecond):
			fmt.Fprintf(lb.stderr(), "littleboss: lameduck mode, exiting in %v\n", lb.LameduckTimeout)
		}
		if !stopped {
			exitCode = <-stopSignal
		}
		lb.exit(exitCode)
	}()

	lb.mu.Lock()
	lb.status = status{
		BossStart: time.Now(),
		BossPid:   os.Getpid(),
		Listeners: make(map[string]string),
	}
	lb.mu.Unlock()

	go lb.runChild(childPath)

	for {
		conn, err := ln.AcceptUnix()
		if err != nil {
			ln.Close()
			lb.Logf("%v", err) // TODO remove?
			lb.exit(1)
		}
		go lb.handler(conn)
	}
}

// handler handles socket connections to a littleboss supervisor.
// These can be commands to stop, reload, or provide status.
func (lb *Littleboss) handler(conn *net.UnixConn) {
	defer conn.Close()
	r := json.NewDecoder(conn)
	w := json.NewEncoder(conn)

	for {
		var req ipcReq
		if err := r.Decode(&req); err != nil {
			return
		}
		switch req.Cmd {
		case "stop":
			lb.handleStop(w)
		case "reload":
			lb.handleReload(w, req)
		case "status":
			lb.handleStatus(w)
		default:
			lb.Logf("unknown ipc command: %q", req.Cmd)
			return
		}
	}
}

func (lb *Littleboss) handleStatus(w *json.Encoder) {
	lb.Logf("status requested")

	lb.mu.Lock()
	res := ipcRes{
		Type:   "status",
		Status: &lb.status,
	}
	w.Encode(res)
	lb.mu.Unlock()
}

func (lb *Littleboss) handleReload(w *json.Encoder, req ipcReq) {
	lb.Logf("reload requested")
	lb.reload <- req.ChildPath
	go lb.handleStopBegin(nil)
	err := <-lb.reloadDone
	errStr := ""
	if err != nil {
		errStr = err.Error()
	}

	res := ipcRes{
		Type:  "reloaded",
		Error: errStr,
	}
	w.Encode(res)
}

func (lb *Littleboss) handleStop(w *json.Encoder) {
	lb.Logf("stop requested")
	stopSignal := make(chan int)
	lb.handleStopBegin(stopSignal)

	var exitCode int
	stopped := false
	select {
	case exitCode = <-stopSignal:
		stopped = true
	case <-time.After(100 * time.Millisecond):
	}

	if !stopped {
		w.Encode(ipcRes{Type: "stopping"})
		exitCode = <-stopSignal
	}

	res := ipcRes{
		Type:     "stopped",
		ExitCode: exitCode,
	}
	w.Encode(res)
	lb.exit(exitCode)
}

func (lb *Littleboss) handleStopBegin(stopSignal chan int) {
	lb.mu.Lock()
	lb.stopSignal = stopSignal
	process := lb.childProcess
	childPiper := lb.childPiper
	lb.mu.Unlock()

	if process != nil {
		childPiper.Write("lameduck")
		done := make(chan struct{}, 2)
		go func() {
			if childPiper.Read() == "finished" {
				done <- struct{}{}
			}
		}()
		go func() {
			process.Wait()
			done <- struct{}{}
		}()
		go func() {
			select {
			case <-done:
			case <-time.After(lb.LameduckTimeout):
				lb.Logf("timeout, forcing process exit")
				process.Kill()
			}
		}()
	}
}

func (lb *Littleboss) runChild(childPath string) {
	var prevChildPath string
	var flags, prevFlags []string
	var extraFiles, prevExtraFiles []*os.File

	flags, extraFiles = lb.collectFlags()

	loadPrev := func() bool {
		if !lb.FallbackOnFailure {
			return false
		}
		if prevChildPath == "" {
			return false
		}
		childPath = prevChildPath
		flags = prevFlags
		extraFiles = prevExtraFiles
		prevChildPath = ""
		prevFlags = nil
		prevExtraFiles = nil
		return true
	}

	for {
		bossPipeR, childPipeW, err := os.Pipe()
		if err != nil {
			lb.fatalf("cannot create pipe: %v", err)
		}
		childPipeR, bossPipeW, err := os.Pipe()
		if err != nil {
			lb.fatalf("cannot create pipe: %v", err)
		}

		cmd := exec.Command(childPath, flags...)
		cmd.ExtraFiles = append([]*os.File{bossPipeR, bossPipeW}, extraFiles...)
		cmd.Stdin = os.Stdin // TODO: Stdio fields in *Littleboss
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		cmd.SysProcAttr = &syscall.SysProcAttr{
			Setpgid: true, // we propagate SIGINT ourselves
		}

		err = cmd.Start()
		if err != nil {
			lb.Logf("%s failed to start: %v", childPath, err)
		}

		// These fds have bene duped by the forked process.
		bossPipeR.Close()
		bossPipeW.Close()

		var exitCode int
		if err == nil {
			childPiper := newPiper(childPipeR, childPipeW)

			lb.mu.Lock()
			stopSignal := lb.stopSignal
			lb.childProcess = cmd.Process
			lb.childPiper = childPiper
			lb.status.Path = childPath
			lb.status.Args = flags
			lb.status.ChildStart = time.Now()
			lb.status.ChildPid = cmd.Process.Pid
			for name := range lb.status.Listeners {
				delete(lb.status.Listeners, name)
			}
			for name, lnf := range lb.lnFlags {
				if lnf.ln != nil {
					lb.status.Listeners[name] = lnf.ln.Addr().String()
				} else if lnf.pc != nil {
					lb.status.Listeners[name] = lnf.pc.LocalAddr().String()
				}
			}
			lb.mu.Unlock()

			if stopSignal != nil {
				// Oops. A signal to stop came in while we
				// were starting the process.
				// Now it is our job to do the work of killing it.
				cmd.Process.Kill()
			} else {
				if res := childPiper.Read(); res != "ready" {
					err = fmt.Errorf("child not ready: %q (%v)", res, childPiper.Error())
				} else {
					childPiper.Write("go")
					err = childPiper.Error()
				}
			}
		}

		failure := make(chan error, 1)
		go func(err error) {
			if err == nil {
				// We pause for a moment before reporting successful
				// reloads in case the client fails early into its
				// main function (which is common).
				select {
				case err = <-failure:
				case <-time.After(250 * time.Millisecond):
				}
			}

			lb.mu.Lock()
			lb.status.Reloads++
			lb.mu.Unlock()

			select {
			case lb.reloadDone <- err:
			default:
			}
		}(err)

		if err == nil {
			err = cmd.Wait()
		}

		if err != nil {
			failure <- err
			if exErr, _ := err.(*exec.ExitError); exErr != nil {
				exitCode = exErr.Sys().(syscall.WaitStatus).ExitStatus()
				lb.Logf("exit code %d", exitCode)
			} else {
				exitCode = 1
				lb.Logf("exit: %v", err)
			}
		}

		childPipeR.Close()
		childPipeW.Close()

		lb.mu.Lock()
		lb.childProcess = nil
		lb.childPiper = nil
		stopSignal := lb.stopSignal
		lb.stopSignal = nil
		if err != nil {
			lb.status.Failures++
		}
		lb.mu.Unlock()

		if stopSignal != nil {
			stopSignal <- exitCode
			return
		}

		// Handle reloads.
		select {
		case newChildPath := <-lb.reload:
			// Reload was requested, so child exiting was expected.
			// The particular error code does not matter: move forward!
			prevChildPath = childPath
			prevFlags = flags
			prevExtraFiles = extraFiles

			childPath = newChildPath
			flags, extraFiles = lb.collectFlags()
			continue
		default:
		}

		if err == nil {
			// Reload was not requested and child exited cleanly.
			if lb.Persist {
				lb.Logf("%s exited, waiting one second and restarting", childPath)
				time.Sleep(1 * time.Second)
				continue
			}
			lb.Logf("%s exited, shutting down", childPath)
			lb.exit(0)
		}

		// Reload was not requested and process exited badly.
		oldChildPath := childPath
		if loadPrev() {
			lb.Logf("%s failed: %v, restarting previous binary", oldChildPath, err)
			continue
		} else {
			if lb.Persist {
				time.Sleep(1 * time.Second)
				lb.Logf("%s failed, waiting one second and restarting", childPath)
				continue
			}
			// Child failed, no old binary, no persistence, so exit the boss.
			lb.fatalf("%v", err)
		}
	}
}

func (lb *Littleboss) collectFlags() (flags []string, extraFDs []*os.File) {
	addLn := func(f *flag.Flag, lnf *ListenerFlag) {
		net := lnf.net[:3]
		// 0 stdin, 1 stdout, 2 stderr, 3 bossPipeR, 4 bossPipeW, 5+ listeners
		flags = append(flags, fmt.Sprintf("-%s=%s:fd:%s:%d", f.Name, f.Value, net, 5+len(extraFDs)))
		extraFDs = append(extraFDs, lnf.f)
	}

	flags = []string{"-" + lb.modeFlagName + "=child"}

	visitedLns := make(map[string]bool)
	lb.FlagSet.Visit(func(f *flag.Flag) {
		if lnf := lb.lnFlags[f.Name]; lnf != nil {
			visitedLns[f.Name] = true
			if lnf.ln == nil && lnf.pc == nil {
				// Listener is disabled. We pass the flag,
				// as the value is non-default, but do not
				// pass an FD.
				flags = append(flags, "-"+f.Name+"=")
			} else {
				addLn(f, lnf)
			}
		} else if f.Name == lb.modeFlagName {
			return // rewritten first
		} else {
			flags = append(flags, "-"+f.Name+"="+f.Value.String())
		}
	})

	// Any unset listener flag with a non-empty default value needs
	// to be handled, even though it was not enumerated by Visit above.
	var extraLnFlags []string
	for name := range lb.lnFlags {
		if !visitedLns[name] {
			extraLnFlags = append(extraLnFlags, name)
		}
	}
	sort.Strings(extraLnFlags)
	for _, name := range extraLnFlags {
		lnf := lb.lnFlags[name]
		if lnf.ln == nil && lnf.pc == nil {
			continue
		}
		addLn(lb.FlagSet.Lookup(name), lnf)
	}

	return flags, extraFDs
}

type ipcReq struct {
	Cmd       string // "stop", "status", "reload"
	ChildPath string `json:",omitempty"`
}

type ipcRes struct {
	Type     string  // "stopping", "stopped", "status", "reloading", "reloaded"
	Error    string  `json:",omitempty"`
	ExitCode int     `json:",omitempty"`
	Status   *status `json:",omitempty"`
}

type status struct {
	BossStart  time.Time
	BossPid    int
	ChildStart time.Time
	ChildPid   int
	Reloads    int
	Failures   int
	Path       string
	Args       []string
	Listeners  map[string]string // name -> addr
}

// issueStop sends a "stop" instruction to a running supervisor.
func (lb *Littleboss) issueStop() {
	socketpath := filepath.Join(lb.socketdir(), lb.name+".socket")
	conn, err := net.Dial("unix", socketpath)
	if err != nil {
		lb.fatalf("cannot connect to %s: %v", lb.name, err)
	}
	r := json.NewDecoder(conn)
	w := json.NewEncoder(conn)

	if err := w.Encode(ipcReq{Cmd: "stop"}); err != nil {
		lb.fatalf("ipc write error: %v", err)
	}

	for {
		var res ipcRes
		if err := r.Decode(&res); err != nil {
			lb.fatalf("ipc read error: %v", err)
		}
		switch res.Type {
		case "stopping":
			fmt.Fprintf(lb.stderr(), "stopping %s\n", lb.name)
		case "stopped":
			fmt.Fprintf(lb.stderr(), "stopped %s exit code %d\n", lb.name, res.ExitCode)
			os.Exit(res.ExitCode)
		}
	}
}

// issueReload sends a "reload" instruction to a running supervisor.
func (lb *Littleboss) issueReload() {
	socketpath := filepath.Join(lb.socketdir(), lb.name+".socket")
	conn, err := net.Dial("unix", socketpath)
	if err != nil {
		lb.fatalf("cannot connect to %s: %v", lb.name, err)
	}
	r := json.NewDecoder(conn)
	w := json.NewEncoder(conn)

	childPath, err := os.Executable()
	if err != nil {
		lb.fatalf("cannot find oneself: %v", err)
	}

	req := ipcReq{
		Cmd:       "reload",
		ChildPath: childPath,
	}
	if err := w.Encode(req); err != nil {
		lb.fatalf("ipc write error: %v", err)
	}

	for {
		var res ipcRes
		if err := r.Decode(&res); err != nil {
			lb.fatalf("ipc read error: %v", err)
		}
		switch res.Type {
		case "reloading":
			fmt.Fprintf(lb.stderr(), "reloading %s\n", lb.name)
		case "reloaded":
			if res.Error == "" {
				fmt.Fprintf(lb.stderr(), "reloaded %s\n", lb.name)
				os.Exit(0)
			} else {
				fmt.Fprintf(lb.stderr(), "reload of %s failed: %s\n", lb.name, res.Error)
				os.Exit(1)
			}
		}
	}
}

// issueStatus sends a "status" instruction to a running supervisor.
func (lb *Littleboss) issueStatus() {
	socketpath := filepath.Join(lb.socketdir(), lb.name+".socket")
	conn, err := net.Dial("unix", socketpath)
	if err != nil {
		lb.fatalf("cannot connect to %s: %v", lb.name, err)
	}
	r := json.NewDecoder(conn)
	w := json.NewEncoder(conn)

	if err := w.Encode(ipcReq{Cmd: "status"}); err != nil {
		lb.fatalf("ipc write error: %v", err)
	}

	var res ipcRes
	if err := r.Decode(&res); err != nil {
		lb.fatalf("ipc read error: %v", err)
	}
	if res.Type != "status" {
		lb.fatalf("expected status, got %q\n", res.Type)
	}
	status := res.Status

	bossStart := status.BossStart.Format("2006-01-02 15:04:05")
	childStart := status.ChildStart.Format("2006-01-02 15:04:05")

	fmt.Fprintf(os.Stdout, "littleboss: %s\n\n", lb.name)
	fmt.Fprintf(os.Stdout, "boss  pid %d, started %s\n", status.BossPid, bossStart)
	fmt.Fprintf(os.Stdout, "child pid %d, started %s, reloads %d, failures %d\n\n", status.ChildPid, childStart, status.Reloads, status.Failures)
	fmt.Fprintf(os.Stdout, "command line:\n")
	fmt.Fprintf(os.Stdout, "\t%s %s\n", status.Path, strings.Join(status.Args, " "))
	if len(status.Listeners) > 0 {
		fmt.Fprintf(os.Stdout, "\nlisteners:\n")
		for name, addr := range status.Listeners {
			fmt.Fprintf(os.Stdout, "\t%s:\t%s\n", name, addr)
		}
	}

	os.Exit(0)
}

// child is executed in the child process.
// It prepares the listener flags, connects the
// root context to the supervisor and calls mainFn.
func (lb *Littleboss) child(mainFn func(ctx context.Context)) {
	// We are in the child process.
	lb.running = true
	for name, lnf := range lb.lnFlags {
		if lnf.val == "" {
			continue
		}
		i := strings.LastIndex(lnf.val, ":fd:")
		if i < 0 {
			panic(fmt.Sprintf("littleboss: child listener flag %q value does not include fd: %q", name, lnf.val))
		}
		i += len(":fd:")
		end := strings.LastIndexByte(lnf.val[i:], ':')
		network := lnf.val[i : i+end]
		fd, err := strconv.Atoi(lnf.val[i+end+1:])
		if err != nil {
			panic(fmt.Sprintf("littleboss: child listener flag %q bad value: %q: %v", name, lnf.val, err))
		}
		lnf.f = os.NewFile(uintptr(fd), name)
		switch {
		case strings.HasPrefix(network, "tcp"):
			lnf.ln, err = net.FileListener(lnf.f)
		case strings.HasPrefix(network, "udp"), strings.HasPrefix(network, "ip"):
			lnf.pc, err = net.FilePacketConn(lnf.f)
		default:
			err = errors.New("unknown network type")
		}
		if err != nil {
			panic(fmt.Sprintf("littleboss: child listener flag %q (%q) is no good: %v", name, lnf.val, err))
		}
	}

	ctx, cancel := context.WithCancel(context.Background())

	bossPiper := newPiper(os.NewFile(3, "bossPipeR"), os.NewFile(4, "bossPipeW"))
	bossPiper.Write("ready")
	if err := bossPiper.Error(); err != nil {
		panic(fmt.Sprintf("littleboss: cannot signal boss: %v", err))
	}
	if res := bossPiper.Read(); res != "go" {
		panic(fmt.Sprintf("littleboss: child did not get go: %s (%v)", res, bossPiper.Error()))
	}

	go func() {
		switch cmd := bossPiper.Read(); cmd {
		case "lameduck":
			cancel()
		default:
			panic(fmt.Sprintf("littleboss: unknown boss pipe command: %s (%v)", cmd, bossPiper.Error()))
		}
	}()

	mainFn(ctx)
	bossPiper.Write("finished")
	lb.exit(0)
}

func (lb *Littleboss) stderr() io.Writer {
	return lb.FlagSet.Output()
}

func (lb *Littleboss) socketdir() string {
	return filepath.Join(os.TempDir(), fmt.Sprintf("littleboss-%s-%s", lb.username, lb.name))
}

func (lb *Littleboss) fatalf(format string, args ...interface{}) {
	fmt.Fprintf(lb.stderr(), "%s: littleboss: %s\n", lb.cmdname, fmt.Sprintf(format, args...))
	lb.exit(1)
}

func (lb *Littleboss) isSupervisor() bool {
	return lb.mode != nil && *lb.mode == "start"
}

func (lb *Littleboss) exit(code int) {
	if lb.isSupervisor() {
		os.RemoveAll(lb.socketdir())
	}
	os.Exit(code)
}

type piper struct {
	r, w *os.File
	bufr *bufio.Reader
	err  error
}

func newPiper(r, w *os.File) *piper {
	return &piper{
		r:    r,
		w:    w,
		bufr: bufio.NewReader(r),
	}
}

func (p *piper) Error() error {
	return p.err
}

func (p *piper) Read() string {
	if p.err != nil {
		return ""
	}
	str, err := p.bufr.ReadString('\n')
	if err != nil {
		p.r.Close()
		p.w.Close()
		p.err = err
		return ""
	}
	return str[:len(str)-1]
}

func (p *piper) Write(s string) {
	if p.err != nil {
		return
	}
	b := make([]byte, len(s)+1)
	copy(b, s)
	b[len(b)-1] = '\n'
	_, p.err = p.w.Write(b)
	if p.err != nil {
		p.r.Close()
		p.w.Close()
	}
}

func isValidServiceName(name string) bool {
	for _, r := range name {
		if !isASCIILetter(r) && !isDigit(r) && r != '_' && r != '-' {
			return false
		}
	}
	return true
}

func isASCIILetter(r rune) bool { return 'a' <= r && r <= 'z' || 'A' <= r && r <= 'Z' }
func isDigit(r rune) bool       { return '0' <= r && r <= '9' }
