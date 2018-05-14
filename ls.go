package main

import (
	"fmt"
	"log"
	"os"
	"sort"

	"crawshaw.io/ltboss/rpc"
)

func ls(args []string) {
	clients, err := FindDaemons()
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s ls: %v\n", cmdname, err)
		os.Exit(1)
	}

	ch := make(chan *rpc.InfoResponse, len(clients))
	for _, client := range clients {
		client := client
		go func() {
			info, err := client.Info()
			if err != nil {
				log.Printf("%s: %v", client.SocketPath, err)
			}
			ch <- info
		}()
	}
	var infos []*rpc.InfoResponse
	for range clients {
		if info := <-ch; info != nil {
			infos = append(infos, info)
		}
	}

	sort.Slice(infos, func(i, j int) bool { return infos[i].ServiceName < infos[j].ServiceName })
	for _, info := range infos {
		fmt.Printf("%s\n", info.ServiceName)
	}
	os.Exit(0)
}
