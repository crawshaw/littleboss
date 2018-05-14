package main

import (
	"bufio"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"crawshaw.io/littleboss/lbclient"
)

func start(args []string) {
	args = args[1:]

	flagSet := flag.NewFlagSet("littleboss-start", 0)
	flagName := flagSet.String("name", "", "service name")
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
			names[info.ServiceName] = true
		}

		name = filepath.Base(binpath)
		for i := 1; names[name]; i++ {
			name = fmt.Sprintf("%s-%d", filepath.Base(binpath), i)
		}
	}

	r, w, err := os.Pipe()
	if err != nil {
		fatalf("%v", err)
	}

	cmd := exec.Command(cmdpath, "-daemon", "-name="+name)
	cmd.Stdout = w
	//cmd.Stderr = os.Stderr // TODO remove
	if err := cmd.Start(); err != nil {
		fatalf("%v", err)
	}

	sockFileStr, err := bufio.NewReader(r).ReadString('\n')
	if err != nil {
		fatalf("bad daemon read: %v", err)
	}
	log.Print(sockFileStr)
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

	c, err := lbclient.NewClient(socketpath)
	if err != nil {
		fatalf("%v", err)
	}

	c.Start(binpath, args)

	fmt.Println(c.Info())

	os.Exit(0)
}
