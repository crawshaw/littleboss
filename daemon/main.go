package daemon // import "crawshaw.io/littleboss/daemon"

import (
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

	"crawshaw.io/littleboss/rpc"
)

var flagSet = flag.NewFlagSet("daemon flags", 0)
var flagName = flagSet.String("name", "", "service name")

var state = struct {
	mu           sync.Mutex
	bossStart    time.Time
	bossPID      int
	serviceStart time.Time
	serviceProc  *os.Process
}{
	bossStart: time.Now(),
	bossPID:   int(syscall.Getpid()),
}

var socketpath = ""
var socketln *net.UnixListener

func Main() {
	if err := flagSetParse(); err != nil {
		fmt.Fprintf(os.Stderr, "littleboss daemon: %v\n", err)
		exit(1)
	}
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
		var req rpc.Request
		if err := r.Decode(&req); err != nil {
			log.Printf("connection closed: %v", err)
			break
		}
		var res interface{}
		var err error
		switch req.Type {
		case "info":
			res, err = handlerInfo(&req)
		case "start":
			res, err = handlerStart(&req)
		default:
			err = fmt.Errorf("unknown request type: %q", req.Type)
		}
		if err != nil {
			w.Encode(rpc.ErrResponse{Error: err.Error()})
			log.Printf("closing on error: %v", err)
			return
		}
		if w.Encode(res) != nil {
			return
		}
	}
}

func handlerInfo(req *rpc.Request) (interface{}, error) {
	state.mu.Lock()
	defer state.mu.Unlock()

	res := rpc.InfoResponse{
		ServiceName: *flagName,
		BossStart:   state.bossStart,
		BossPID:     state.bossPID,
	}

	return res, nil
}

func handlerStart(req *rpc.Request) (interface{}, error) {
	state.mu.Lock()
	defer state.mu.Unlock()

	if state.serviceProc != nil {
		return nil, fmt.Errorf("service already started")
	}

	cmd := exec.Command(req.Binary, req.Args...)
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	state.serviceProc = cmd.Process

	return &rpc.StartResponse{
		ServicePID: state.serviceProc.Pid,
	}, nil
}
