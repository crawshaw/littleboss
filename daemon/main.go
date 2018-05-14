package daemon // import "crawshaw.io/littleboss/daemon"

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"crawshaw.io/littleboss/lbrpc"
)

var flagSet = flag.NewFlagSet("daemon flags", 0)
var flagName = flagSet.String("name", "", "service name")

var state struct {
	ctx      context.Context
	cancelFn func()

	mu           sync.Mutex
	bossStart    time.Time
	bossPID      int
	serviceStart time.Time
	serviceProc  *os.Process
	exitDone     chan struct{} // closed when process closes, exitCode avail
	exitCode     int
}

var socketpath = ""
var socketln *net.UnixListener

func Main() {
	state.ctx, state.cancelFn = context.WithCancel(context.Background())

	state.mu.Lock()
	state.bossStart = time.Now()
	state.bossPID = int(syscall.Getpid())
	state.mu.Unlock()

	if err := flagSetParse(); err != nil {
		fmt.Fprintf(os.Stderr, "littleboss daemon: %v\n", err)
		exit(1)
	}
	log.SetPrefix("littleboss:" + *flagName + ": ")

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigCh
		fmt.Fprintf(os.Stderr, "signal received: %s\n", sig)
		exit(3)
	}()

	dirID := make([]byte, 8)
	if _, err := rand.Read(dirID); err != nil {
		fmt.Fprintf(os.Stderr, "littleboss daemon: %v\n", err)
		exit(1)
	}

	socketpath = filepath.Join(os.TempDir(), fmt.Sprintf("littleboss-%x/littleboss.%d", dirID, syscall.Getpid()))

	if err := os.Mkdir(filepath.Dir(socketpath), 0700); err != nil {
		fmt.Fprintf(os.Stderr, "littleboss daemon: %v\n", err)
		exit(1)
	}

	addr, err := net.ResolveUnixAddr("unix", socketpath) // "unix" == SOCK_STREAM
	if err != nil {
		fmt.Fprintf(os.Stderr, "littleboss daemon: %v\n", err)
		exit(1)
	}
	socketln, err = net.ListenUnix("unix", addr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "littleboss daemon: %v\n", err)
		exit(1)
	}
	if f, _ := socketln.File(); f != nil {
		f.Chmod(0700) // TODO test this
	}

	fmt.Fprintf(os.Stdout, "LITTLEBOSS_SOCK_FILE=%q\n", socketpath)

	for {
		conn, err := socketln.AcceptUnix()
		if err != nil {
			log.Print(err)
			exit(1)
		}
		go handler(conn)
	}
}

func flagSetParse() error {
	if err := flagSet.Parse(os.Args[2:]); err != nil {
		return err
	}
	if flagSet.NArg() > 0 {
		return fmt.Errorf("excess arguments: %v\n", os.Args[len(os.Args)-flagSet.NArg():])
	}
	if *flagName == "" {
		return fmt.Errorf("no -name")
	}
	return nil
}

func exit(code int) {
	if socketln != nil {
		socketln.Close() // calls unlink
	}
	if socketpath != "" {
		os.RemoveAll(filepath.Dir(socketpath))
	}

	state.mu.Lock()
	if state.serviceProc != nil {
		state.serviceProc.Kill()
	}
	state.mu.Unlock()

	os.Exit(code)
}

func handler(conn *net.UnixConn) {
	defer conn.Close()
	r := json.NewDecoder(conn)
	w := json.NewEncoder(conn)

	log.Printf("new connection")
	// TODO: consider SO_PEERCRED for double-checking the uid, and pids in logs

	for {
		var req lbrpc.Request
		if err := r.Decode(&req); err != nil {
			log.Printf("connection closed: %v", err)
			break
		}
		var res interface{}
		var err error
		switch req.Type {
		case "info":
			res, err = handleInfo(&req)
		case "start":
			res, err = handleStart(&req)
		case "stop":
			res, err = handleStop(&req)
			defer exit(0)
		default:
			err = fmt.Errorf("unknown request type: %q", req.Type)
		}
		if err != nil {
			w.Encode(lbrpc.ErrResponse{Error: err.Error()})
			log.Printf("closing on error: %v", err)
			return
		}
		if w.Encode(res) != nil {
			return
		}
	}
}

func handleInfo(req *lbrpc.Request) (interface{}, error) {
	state.mu.Lock()
	defer state.mu.Unlock()

	res := lbrpc.InfoResponse{
		ServiceName: *flagName,
		BossStart:   state.bossStart,
		BossPID:     state.bossPID,
	}

	return res, nil
}

func handleStart(req *lbrpc.Request) (interface{}, error) {
	state.mu.Lock()
	defer state.mu.Unlock()

	if state.serviceProc != nil {
		return nil, fmt.Errorf("service already started")
	}

	return handleStartLocked(req)
}

func handleStartLocked(req *lbrpc.Request) (interface{}, error) {
	state.serviceStart = time.Now()
	cmd := exec.Command(req.Binary, req.Args...)
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	state.serviceProc = cmd.Process

	go func() {
		err := cmd.Wait()
		code := 0
		if err != nil {
			if exErr, _ := err.(*exec.ExitError); exErr != nil {
				code = exErr.Sys().(syscall.WaitStatus).ExitStatus()
				log.Printf("exit code %d", code)
			} else {
				log.Printf("exit: %v", err)
			}
		}

		state.mu.Lock()
		state.serviceProc = nil
		state.exitCode = code
		if state.exitDone != nil {
			close(state.exitDone)
			state.exitDone = nil
		}
		state.mu.Unlock()

		go func() {
			select {
			case <-state.ctx.Done():
				return
			case <-time.After(1 * time.Second):
				state.mu.Lock()
				defer state.mu.Unlock()

				if state.serviceProc != nil {
					// Manual user restart, disappear.
					return
				}

				_, err := handleStartLocked(req)
				if err != nil {
					log.Printf("cannot restart service: %v", err)
					exit(1)
				}
			}
		}()
	}()

	return &lbrpc.StartResponse{
		ServicePID: state.serviceProc.Pid,
	}, nil
}

func handleStop(req *lbrpc.Request) (interface{}, error) {
	select {
	case <-state.ctx.Done():
		return nil, fmt.Errorf("service already stopping")
	default:
	}
	state.cancelFn()

	res := new(lbrpc.StopResponse)

	state.mu.Lock()
	if state.serviceProc == nil {
		state.mu.Unlock()
		return nil, fmt.Errorf("service not running")
	}
	exitDone := make(chan struct{})
	state.exitDone = exitDone
	timeout := req.Timeout
	err := state.serviceProc.Signal(syscall.SIGINT)
	if err != nil {
		res.InterruptFailed = true
		timeout = 0 // no point waiting
	}
	state.mu.Unlock()

	select {
	case <-time.After(timeout):
		// process still running, force it out
		state.mu.Lock()
		if state.serviceProc != nil {
			res.Forced = true
			state.serviceProc.Kill()
		}
		state.mu.Unlock()

		select {
		case <-exitDone:
			// process has exited, exit code is available
		}
	case <-exitDone:
		// process exited cleanly from lame duck
	}

	state.mu.Lock()
	res.ExitCode = state.exitCode
	state.mu.Unlock()

	return res, nil
}
