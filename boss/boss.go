package boss // import "crawshaw.io/littleboss/boss"

import (
	"context"
	"crypto/rand"
	"encoding/json"
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

var boss struct {
	ctx        context.Context
	cancelFn   func()
	name       string
	start      time.Time
	pid        int
	detached   bool // no stdio is wired up to the service process
	socketpath string
	socketln   *net.UnixListener
}

var state struct {
	mu       sync.Mutex
	start    time.Time
	proc     *os.Process
	binary   string
	args     []string
	exitDone chan struct{} // closed when process closes, exitCode avail
	exitCode int
}

// Main starts a littleboss service.
// If detached, no stdio is wired up to the service process.
// It does not return.
func Main(name, binary string, args []string, detached bool) {
	boss.ctx, boss.cancelFn = context.WithCancel(context.Background())
	boss.start = time.Now()
	boss.pid = int(syscall.Getpid())
	boss.detached = detached
	boss.name = name

	log.SetPrefix("littleboss:" + boss.name + ": ")
	log.SetFlags(0)

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

	boss.socketpath = filepath.Join(os.TempDir(), fmt.Sprintf("littleboss-%x/littleboss.%d", dirID, syscall.Getpid()))

	if err := os.Mkdir(filepath.Dir(boss.socketpath), 0700); err != nil {
		fmt.Fprintf(os.Stderr, "littleboss daemon: %v\n", err)
		exit(1)
	}

	addr, err := net.ResolveUnixAddr("unix", boss.socketpath) // "unix" == SOCK_STREAM
	if err != nil {
		fmt.Fprintf(os.Stderr, "littleboss daemon: %v\n", err)
		exit(1)
	}
	boss.socketln, err = net.ListenUnix("unix", addr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "littleboss daemon: %v\n", err)
		exit(1)
	}
	if f, _ := boss.socketln.File(); f != nil {
		f.Chmod(0700) // TODO test this
	}

	// TODO: no need to use the wire structure for the start command.
	req := &lbrpc.Request{
		Type:   "start",
		Binary: binary,
		Args:   args,
	}
	state.mu.Lock()
	_, err = handleStartLocked(req)
	state.mu.Unlock()

	if err != nil {
		fmt.Fprintf(os.Stderr, "littleboss: start failed: %v\n", err)
		exit(1)
	}

	if detached {
		fmt.Fprintf(os.Stdout, "LITTLEBOSS_SOCK_FILE=%q\n", boss.socketpath)
	} else {
		log.Printf("socket path: %s", boss.socketpath)
	}

	for {
		conn, err := boss.socketln.AcceptUnix()
		if err != nil {
			//log.Print(err)
			exit(1)
		}
		go handler(conn)
	}
}

func exit(code int) {
	if boss.socketln != nil {
		boss.socketln.Close() // calls unlink
	}
	if boss.socketpath != "" {
		os.RemoveAll(filepath.Dir(boss.socketpath))
	}

	state.mu.Lock()
	if state.proc != nil {
		state.proc.Kill()
	}
	state.mu.Unlock()

	os.Exit(code)
}

func handler(conn *net.UnixConn) {
	defer conn.Close()
	r := json.NewDecoder(conn)
	w := json.NewEncoder(conn)

	// TODO: consider SO_PEERCRED for double-checking the uid, and pids in logs

	for {
		var req lbrpc.Request
		if err := r.Decode(&req); err != nil {
			break
		}
		var res interface{}
		var err error
		switch req.Type {
		case "info":
			res, err = handleInfo(&req)
		case "reload":
			res, err = handleReload(&req)
		case "stop":
			res, err = handleStop(&req)
			defer exit(0) // we return below, which triggers this process exit
		default:
			err = fmt.Errorf("unknown request type: %q", req.Type)
		}
		if err != nil {
			w.Encode(lbrpc.ErrResponse{Error: err.Error()})
			return
		}
		if w.Encode(res) != nil {
			return
		}
		if req.Type == "stop" {
			return
		}
	}
}

func handleInfo(req *lbrpc.Request) (interface{}, error) {
	state.mu.Lock()
	defer state.mu.Unlock()

	res := lbrpc.InfoResponse{
		Name:      boss.name,
		PID:       0, // TODO
		Start:     state.start,
		Binary:    state.binary,
		Args:      state.args,
		BossStart: boss.start,
		BossPID:   boss.pid,
	}

	return res, nil
}

func handleStartLocked(req *lbrpc.Request) (interface{}, error) {
	state.start = time.Now()
	log.Printf("start %s %v", req.Binary, req.Args)
	cmd := exec.Command(req.Binary, req.Args...)
	if !boss.detached {
		cmd.Stdin = os.Stdin
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	state.proc = cmd.Process
	state.binary = req.Binary
	state.args = req.Args

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
		state.proc = nil
		state.exitCode = code
		exitDone := state.exitDone
		state.exitDone = nil
		state.mu.Unlock()

		if exitDone != nil {
			close(exitDone)
			return
		}

		// Restart the service.
		go func() {
			select {
			case <-boss.ctx.Done():
				return
			case <-time.After(1 * time.Second):
			}

			state.mu.Lock()
			defer state.mu.Unlock()

			if state.proc != nil {
				// Manual user restart, disappear.
				return
			}

			_, err := handleStartLocked(req)
			if err != nil {
				log.Printf("cannot restart service: %v", err)
				exit(1)
			}
		}()
	}()

	return &lbrpc.StartResponse{
		ServicePID: state.proc.Pid,
	}, nil
}

func killService(timeout time.Duration) (forced bool, err error) {
	state.mu.Lock()
	if state.proc == nil {
		state.mu.Unlock()
		return false, fmt.Errorf("service not running")
	}
	exitDone := make(chan struct{})
	state.exitDone = exitDone
	if timeout == 0 {
		timeout = 2 * time.Second
	}
	if err := state.proc.Signal(syscall.SIGINT); err != nil {
		timeout = 0 // no point waiting
	}
	state.mu.Unlock()

	select {
	case <-time.After(timeout):
		// process still running, force it out
		state.mu.Lock()
		if state.proc != nil {
			forced = true
			state.proc.Kill()
		}
		state.mu.Unlock()

		select {
		case <-exitDone:
			// process has exited, exit code is available
		}
	case <-exitDone:
		// process exited cleanly from lame duck
	}

	return forced, nil
}

func handleStop(req *lbrpc.Request) (interface{}, error) {
	select {
	case <-boss.ctx.Done():
		return nil, fmt.Errorf("service already stopping")
	default:
	}
	boss.cancelFn()

	res := new(lbrpc.StopResponse)

	forced, err := killService(req.Timeout)
	if err != nil {
		return nil, err
	}
	res.Forced = forced

	state.mu.Lock()
	res.ExitCode = state.exitCode
	state.mu.Unlock()

	return res, nil
}

func handleReload(req *lbrpc.Request) (interface{}, error) {
	select {
	case <-boss.ctx.Done():
		return nil, fmt.Errorf("service is stopping")
	default:
	}

	res := new(lbrpc.ReloadResponse)

	forced, err := killService(req.Timeout)
	if err != nil {
		return nil, err
	}
	res.Forced = forced

	state.mu.Lock()
	defer state.mu.Unlock()

	if state.proc != nil {
		return nil, fmt.Errorf("reload interrupted")
	}

	if _, err := handleStartLocked(req); err != nil {
		return nil, err
	}

	return res, nil
}
