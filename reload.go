package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	"crawshaw.io/littleboss/lbclient"
)

func reload(args []string) {
	args = args[1:]

	flagSet := flag.NewFlagSet("littleboss-reload", 0)
	flagTimeout := flagSet.Duration("timeout", 2*time.Second, "lame-duck period")
	if err := flagSet.Parse(args); err != nil {
		fatalf("%v", err)
	}

	args = args[len(args)-flagSet.NArg():]
	if len(args) == 0 {
		fatalf("no service executable provided")
	}
	name, binpath, args := args[0], args[1], args[2:]

	client, err := lbclient.FindDaemon(name)
	if err != nil {
		fatalf("stop: %v", err)
	}
	res, err := client.Reload(binpath, args, *flagTimeout)
	if err != nil {
		fatalf("reload: %v", err)
	}
	if res.Forced {
		fmt.Printf("littleboss: lame duck timeout expired, process killed\n")
	}
	if res.ExitCode != 0 {
		fmt.Printf("exit code: %d", res.ExitCode)
	}
	if res.ExitCode != 0 {
		fmt.Printf("process reloaded")
	}
	os.Exit(0)
}
