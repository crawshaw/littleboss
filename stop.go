package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	"crawshaw.io/littleboss/lbclient"
)

func stop(args []string) {
	args = args[1:]

	flagSet := flag.NewFlagSet("littleboss-stop", 0)
	flagTimeout := flagSet.Duration("timeout", 2*time.Second, "lame-duck period")
	if err := flagSet.Parse(args); err != nil {
		fatalf("%v", err)
	}

	args = args[len(args)-flagSet.NArg():]
	if len(args) == 0 {
		fatalf("missing service name")
	}
	if len(args) > 1 {
		fatalf("excess arguments")
	}

	client, err := lbclient.FindDaemon(args[0])
	if err != nil {
		fatalf("stop: %v", err)
	}
	res, err := client.Stop(*flagTimeout)
	if err != nil {
		fatalf("stop: %v", err)
	}
	if res.Forced {
		fmt.Printf("littleboss: lame duck timeout expired, process killed\n")
	}
	if res.ExitCode != 0 {
		fmt.Printf("exit code: %d", res.ExitCode)
	}
	os.Exit(0)
}
