// Package littleboss creates self-supervising Go binaries.
//
// A self-supervising binary starts itself as a child process.
// The parent process becomes a little boss that is responsible for
// mointoring, managing I/O, restarting, and reloading the child.
//
// Convert a program to use littleboss my modifying the main function:
//
//	func main() {
//		lb := littlebosssNew("service-name", nil)
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
package littleboss

// TODO status
// TODO bring child up before shutting down old binary

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
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
	Logf              func(format string, args ...interface{})
	FallbackOnFailure bool
	ShutdownTimeout   time.Duration

	// Fields set once in New.
	name     string
	cmdname  string
	cmd      *string // pointed-to val fixed by flag.Parse in Run.
	username string
	flagSet  *flag.FlagSet
	reload   chan string

	// Fields fixed by Run.
	running bool
	lnFlags map[string]*ListenerFlag

	// Fields used by runChild and "status"/"stop"/"reload" handlers.
	mu           sync.Mutex
	childProcess *os.Process
	childStart   time.Time
	childEnd     time.Time
	childPiper   *piper
	stopSignal   chan int // exitCode
}

const usage = `instruct the littleboss:

start:		start and manage this process using service name %q
start-once:	start and manage this process, do not restart if exits
stop:		signal the littleboss to shutdown the process
status:		print statistics about the running littleboss
reload:		restart the managed process using the executed binary
bypass:		disable littleboss, run the program directly`

type ListenerFlag struct {
	lb  *Littleboss
	net string
	val string
	ln  net.Listener
	f   *os.File // listener fd in children
}

func (lnf *ListenerFlag) String() string {
	if lnf.lb != nil && *lnf.lb.cmd == "child" {
		if i := strings.LastIndex(lnf.val, ":fd:"); i >= 0 {
			return lnf.val[:i]
		}
	}
	return lnf.val
}

func (lnf *ListenerFlag) Network() string        { return lnf.net }
func (lnf *ListenerFlag) Listener() net.Listener { return lnf.ln }
func (lnf *ListenerFlag) Set(value string) error {
	lnf.val = value
	return nil
}

func New(serviceName string, flagSet *flag.FlagSet) *Littleboss {
	if flagSet == nil {
		flagSet = flag.CommandLine
	}
	if !isValidServiceName(serviceName) {
		panic(fmt.Sprintf("littleboss: invalid service name %q, must be [a-zA-Z0-9_-]", serviceName))
	}
	if flagSet.Parsed() {
		panic(fmt.Sprintf("littleboss: flags parsed before New called"))
	}
	uid := os.Geteuid()
	u, err := user.LookupId(strconv.Itoa(uid))
	if err != nil {
		panic(fmt.Sprintf("littleboss: cannot determine user name: %v", err))
	}

	cmd := flagSet.String("littleboss", "bypass", fmt.Sprintf(usage, serviceName))

	lb := &Littleboss{
		FallbackOnFailure: true,
		ShutdownTimeout:   2 * time.Second,

		name:     serviceName,
		cmdname:  os.Args[0],
		username: u.Username,
		cmd:      cmd,
		flagSet:  flagSet,
		reload:   make(chan string, 1),
		lnFlags:  make(map[string]*ListenerFlag),
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
	lb.flagSet.Var(lnf, flagName, usage)
	return lnf
}

// Run starts the little boss, with mainFn as the entry point to the serice.
// Run does not return.
func (lb *Littleboss) Run(mainFn func(ctx context.Context)) {
	if lb.running {
		panic("littleboss: Run called multiple times")
	}

	if lb.Logf == nil {
		lb.Logf = func(format string, args ...interface{}) {} // do nothing
	}

	if !lb.flagSet.Parsed() {
		lb.flagSet.Parse(os.Args[1:])
	}

	switch *lb.cmd {
	case "stop":
		lb.stop()
	case "reload":
		panic("TODO littleboss reload")
	case "status":
		panic("TODO littleboss status")
	case "child":
		lb.child(mainFn)
	}

	for name, lnf := range lb.lnFlags {
		ln, err := net.Listen(lnf.net, lnf.val)
		if err != nil {
			fmt.Fprintf(lb.stderr(), "%s: -%s: %v", lb.cmdname, name, err)
			os.Exit(1)
		}
		switch ln := ln.(type) {
		case *net.TCPListener:
			f, err := ln.File()
			if err != nil {
				lb.fatalf("could not get TCP listener fd: %v", err)
			}
			lnf.f = f
		default:
			lb.fatalf("unsupported listener type: %T", ln)
		}
		lnf.ln = ln
	}

	lb.running = true

	switch *lb.cmd {
	case "bypass":
		// Littleboss is not in use, run the program as if we weren't here.
		mainFn(context.Background())
		os.Exit(0)
	case "start-once":
		lb.startBoss(false)
	case "start":
		lb.startBoss(true)
	default:
		fmt.Fprintf(lb.stderr(), "%s: unknown littleboss command: %q\n\n", lb.cmdname, *lb.cmd)
		lb.flagSet.Usage()
		os.Exit(2)
	}

	panic("unexpected exit")
}

func (lb *Littleboss) startBoss(persist bool) {
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

	go lb.runChild(persist, childPath)

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
			lb.Logf("stop requested")
			stopSignal := make(chan int)

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
				select {
				case <-done:
				case <-time.After(2 * time.Second):
					lb.Logf("timeout, forcing process exit")
					process.Kill()
				}
			}

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
		default:
			lb.Logf("unknown ipc command: %q", req.Cmd)
			return
		}
	}
}

func (lb *Littleboss) runChild(persist bool, childPath string) {
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
			lb.childStart = time.Now()
			lb.childPiper = childPiper
			lb.mu.Unlock()

			if stopSignal != nil {
				// Oops. A signal to stop came in while we
				// were starting the process.
				// Now it is our job to do the work of killing it.
				cmd.Process.Kill()
			} else {
				if res := childPiper.Read(); res != "ready" {
					err = fmt.Errorf("child not ready: %q (%v)", res, childPiper.Error())
				}
				childPiper.Write("go")
				err = childPiper.Error()
			}
		}

		if err == nil {
			err = cmd.Wait()
		}

		if err != nil {
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
		lb.childEnd = time.Now()
		lb.childPiper = nil
		stopSignal := lb.stopSignal
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
			if persist {
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
			lb.Logf("%s failed to start: %v, restarting previous binary", oldChildPath, err)
			continue
		} else {
			if persist {
				time.Sleep(1 * time.Second)
				lb.Logf("%s failed to start, waiting one second and restarting", childPath)
				continue
			}
			// Failed to start child, no old binary, no persistence, so exit the boss.
			lb.fatalf("%v", err)
		}
	}
}

func (lb *Littleboss) collectFlags() (flags []string, extraFDs []*os.File) {
	addLn := func(f *flag.Flag, lnf *ListenerFlag) {
		// 0 stdin, 1 stdout, 2 stderr, 3 bossPipeR, 4 bossPipeW, 5+ listeners
		flags = append(flags, "-"+f.Name, fmt.Sprintf("%s:fd:%d", f.Value, 5+len(extraFDs)))
		extraFDs = append(extraFDs, lnf.f)
	}

	visitedLns := make(map[string]bool)
	lb.flagSet.Visit(func(f *flag.Flag) {
		if lnf := lb.lnFlags[f.Name]; lnf != nil {
			visitedLns[f.Name] = true
			addLn(f, lnf)
		} else if f.Name == "littleboss" {
			flags = append(flags, "-littleboss", "child")
		} else {
			flags = append(flags, "-"+f.Name, f.Value.String())
		}
	})

	var extraLnFlags []string
	for name := range lb.lnFlags {
		if !visitedLns[name] {
			extraLnFlags = append(extraLnFlags, name)
		}
	}
	sort.Strings(extraLnFlags)
	for _, name := range extraLnFlags {
		addLn(lb.flagSet.Lookup(name), lb.lnFlags[name])
	}

	return flags, extraFDs
}

type ipcReq struct {
	Cmd       string // "stop", "status", "reload"
	ChildPath string `json:",omitempty"`
}

type ipcRes struct {
	Type     string // "stopping", "stopped", "status", "reloading", "reloaded"
	ExitCode int    `json:",omitempty"`
}

func (lb *Littleboss) stop() {
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

func (lb *Littleboss) child(mainFn func(ctx context.Context)) {
	// We are in the child process.
	lb.running = true
	for name, lnf := range lb.lnFlags {
		i := strings.LastIndex(lnf.val, ":fd:")
		if i < 0 {
			panic(fmt.Sprintf("littleboss: child listener flag %q value does not include fd: %q", name, lnf.val))
		}
		fd, err := strconv.Atoi(lnf.val[i+len(":fd:"):])
		if err != nil {
			panic(fmt.Sprintf("littleboss: child listener flag %q bad value: %q: %v", name, lnf.val, err))
		}
		lnf.f = os.NewFile(uintptr(fd), name)
		lnf.ln, err = net.FileListener(lnf.f)
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
	return lb.flagSet.Output()
}

func (lb *Littleboss) socketdir() string {
	return filepath.Join(os.TempDir(), fmt.Sprintf("littleboss-%s-%s", lb.username, lb.name))
}

func (lb *Littleboss) fatalf(format string, args ...interface{}) {
	fmt.Fprintf(lb.stderr(), "%s: littleboss: %s\n", lb.cmdname, fmt.Sprintf(format, args...))
	lb.exit(1)
}

func (lb *Littleboss) exit(code int) {
	if lb.cmd != nil && (*lb.cmd == "start" || *lb.cmd == "start-once") {
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
