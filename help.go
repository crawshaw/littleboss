package main

import (
	"fmt"
	"os"
	"strings"
)

// help prints `ltboss help` messages and then exits the process.
func help(args []string) {
	switch len(args) {
	case 0: // `ltboss`
		helpcmds()
	case 1: // `ltboss help`
		helpcmds()
		os.Exit(0)
	case 2: // `ltboss help foo`
		for _, cmd := range commands {
			if cmd.name == args[1] {
				helpcmd(cmd)
				os.Exit(0)
			}
		}
		for _, topic := range helpTopics {
			if topic.name == args[1] {
				helptopic(topic)
				os.Exit(0)
			}
		}
		fmt.Fprintf(os.Stderr, "%s: unknown help topic \"%s\"\nRun '%s help'.\n", cmdname, args[1], cmdname)
	default:
		fmt.Fprintf(os.Stderr, "usage: %s help command\nToo many arguments given.\n", cmdname)
	}
	os.Exit(2)
}

func helpcmds() {
	w := os.Stderr
	fmt.Fprintf(w, "LittleBoss is a local service supervisor.\n\nUsage: %s command [arguments]\n\n", cmdname)
	fmt.Fprintf(w, "The commands are:\n")

	colwidth := 0
	for _, cmd := range commands {
		if len(cmd.name) > colwidth {
			colwidth = len(cmd.name)
		}
	}
	for _, topic := range helpTopics {
		if len(topic.name) > colwidth {
			colwidth = len(topic.name)
		}
	}
	colwidth += 3

	for _, cmd := range commands {
		fmt.Fprintf(w, "\t%s%s%s\n", cmd.name, strings.Repeat(" ", colwidth-len(cmd.name)), cmd.oneLiner)
	}
	fmt.Fprintf(w, "\nOther help topics:\n")
	for _, topic := range helpTopics {
		fmt.Fprintf(w, "\t%s%s%s\n", topic.name, strings.Repeat(" ", colwidth-len(topic.name)), topic.oneLiner)
	}
	fmt.Fprintf(w, "\nRun \"%s help [command or topic]\" for details.\n", cmdname)
}

func helpcmd(cmd command) {
	fmt.Fprintf(os.Stderr, "usage: %s %s\n\n", cmdname, cmd.usage)
	fmt.Fprintf(os.Stderr, "%s\n", cmd.docs)
}

func helptopic(topic helpTopic) {
	fmt.Fprintf(os.Stderr, "%s\n", topic.docs)
}

type helpTopic struct {
	name     string
	oneLiner string
	docs     string
}

var helpTopics = []helpTopic{
	{
		name:     "serviceflags",
		oneLiner: `flags accepted by "start" and "reload"`,
		docs: `The service flags are used by both the "start" and "reload" commands:"

	-logfile [path]
		TODO

	-logcmd [binary]
		TODO

	-name [service name]
		TODO
`,
	},
}
