package daemon

import (
	"crypto/rand"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"crawshaw.io/ltboss/rpc"
)

var flagSet = flag.NewFlagSet("daemon flags", 0)

var flagName = flagSet.String("name", "", "service name")

var bossStart = time.Now()
var bossPID = int(syscall.Getpid())

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

var socketpath = ""
var socketln *net.UnixListener

func exit(code int) {
	if socketln != nil {
		socketln.Close() // calls unlink
	}
	if socketpath != "" {
		os.RemoveAll(filepath.Dir(socketpath))
	}
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
		var err error
		switch req.Type {
		case "info":
			err = handlerInfo(w, &req)
		default:
			err = fmt.Errorf("unknown request type: %q", req.Type)
		}
		if err != nil {
			w.Encode(rpc.ErrResponse{Error: err.Error()})
			log.Printf("closing: %v", err)
			break
		}
	}
}

func handlerInfo(w *json.Encoder, req *rpc.Request) error {
	res := rpc.InfoResponse{
		ServiceName: *flagName,
		BossStart:   bossStart,
		BossPID:     bossPID,
	}
	return w.Encode(res)
}

func Main() {
	if err := flagSetParse(); err != nil {
		fmt.Fprintf(os.Stderr, "ltboss daemon: %v\n", err)
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
		fmt.Fprintf(os.Stderr, "ltboss daemon: %v\n", err)
		exit(1)
	}

	socketpath = filepath.Join(os.TempDir(), fmt.Sprintf("ltboss-%x/ltbossd.%d", dirID, syscall.Getpid()))

	if err := os.Mkdir(filepath.Dir(socketpath), 0700); err != nil {
		fmt.Fprintf(os.Stderr, "ltboss daemon: %v\n", err)
		exit(1)
	}

	addr, err := net.ResolveUnixAddr("unix", socketpath) // "unix" == SOCK_STREAM
	if err != nil {
		fmt.Fprintf(os.Stderr, "ltboss daemon: %v\n", err)
		exit(1)
	}
	socketln, err = net.ListenUnix("unix", addr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ltboss daemon: %v\n", err)
		exit(1)
	}
	if f, _ := socketln.File(); f != nil {
		f.Chmod(0700) // TODO test this
	}

	fmt.Fprintf(os.Stdout, "LTBOSS_SOCK_FILE=%q\n", socketpath)

	for {
		conn, err := socketln.AcceptUnix()
		if err != nil {
			log.Print(err)
			exit(1)
		}
		go handler(conn)
	}
}
