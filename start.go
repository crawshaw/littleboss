package main

import (
	"bufio"
	"bytes"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"crawshaw.io/littleboss/daemon"
	"crawshaw.io/littleboss/lbclient"
)

func start(args []string) {
	args = args[1:]

	flagSet := flag.NewFlagSet("littleboss-start", 0)
	flagName := flagSet.String("name", "", "service name")
	flagDetach := flagSet.Bool("detach", false, "start service detached from shell")
	// TODO: serviceflags
	if err := flagSet.Parse(args); err != nil {
		fatalf("%v", err)
	}

	args = args[len(args)-flagSet.NArg():]
	if len(args) == 0 {
		fatalf("no service executable provided")
	}
	binpath := args[0]
	args = args[1:]

	name := *flagName
	if name == "" {
		clients, err := lbclient.FindDaemons()
		if err != nil {
			fatalf("start: survey: %v\n", err)
		}
		names := make(map[string]bool)
		infos := requestInfos(clients)
		for _, info := range infos {
			names[info.Name] = true
		}

		name = filepath.Base(binpath)
		for i := 1; names[name]; i++ {
			name = fmt.Sprintf("%s-%d", filepath.Base(binpath), i)
		}
	}

	if *flagDetach {
		startDetached(name, binpath, args)
	} else {
		daemon.Main(name, binpath, args, false)
	}
}

func startDetached(name, binpath string, args []string) {
	r, w, err := os.Pipe()
	if err != nil {
		fatalf("%v", err)
	}

	stderr := new(bytes.Buffer)

	cmd := exec.Command(cmdpath, "-daemon", name, binpath)
	cmd.Args = append(cmd.Args, args...)
	cmd.Stdout = w
	cmd.Stderr = os.Stderr //stderr
	if err := cmd.Start(); err != nil {
		fatalf("%v", err)
	}

	r.SetReadDeadline(time.Now().Add(2 * time.Second))
	sockFileStr, err := bufio.NewReader(r).ReadString('\n')
	if err != nil {
		os.Stderr.Write(stderr.Bytes())
		fatalf("bad daemon read: %v", err)
	}
	const prefix = `LITTLEBOSS_SOCK_FILE=`
	if !strings.HasPrefix(sockFileStr, prefix) {
		fatalf("bad daemon output: %q", sockFileStr)
	}
	sockFileStr = sockFileStr[len(prefix) : len(sockFileStr)-1]
	socketpath, err := strconv.Unquote(sockFileStr)
	if err != nil {
		fatalf("bad daemon output: %v: %q", err, sockFileStr)
	}
	r.Close()
	w.Close()

	log.Printf("connecting to %q", socketpath)
	c, err := lbclient.NewClient(socketpath)
	if err != nil {
		os.Stderr.Write(stderr.Bytes())
		fatalf("%v", err)
	}

	if _, err := c.Info(); err != nil {
		os.Stderr.Write(stderr.Bytes())
		fatalf("%v", err)
	}
	os.Exit(0)
}
