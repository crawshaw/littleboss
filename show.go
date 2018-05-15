package main

import (
	"fmt"
	"os"
	"strings"
	"time"

	"crawshaw.io/littleboss/lbclient"
)

func show(args []string) {
	args = args[1:]
	if len(args) != 1 {
		fatalf("missing service name")
	}
	client, err := lbclient.FindDaemon(args[0])
	if err != nil {
		fatalf("show: %v\n", err)
	}

	info, err := client.Info()
	if err != nil {
		fatalf("show: info failed: %v", err)
	}

	type field struct {
		name  string
		value string
	}

	uptime := time.Since(info.Start)
	uptime /= time.Second
	uptime *= time.Second

	command := info.Binary
	for _, arg := range info.Args {
		command += " " + arg
	}

	fields := []field{
		{name: "Command", value: command},
		{name: "PID", value: fmt.Sprintf("%d", info.PID)},
		{name: "Uptime", value: fmt.Sprintf("%s", uptime)},
	}
	pad := 0
	for _, f := range fields {
		if len(f.name) > pad {
			pad = len(f.name)
		}
	}
	pad += 3

	fmt.Printf("LittleBoss Service: %s\n\n", info.Name)
	for _, f := range fields {
		fmt.Printf("\t%s:%s%s\n", f.name, strings.Repeat(" ", pad-len(f.name)), f.value)
	}
	fmt.Printf("\n")

	os.Exit(0)
}
