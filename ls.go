package main

import (
	"fmt"
	"log"
	"os"
	"sort"

	"crawshaw.io/littleboss/lbclient"
	"crawshaw.io/littleboss/lbrpc"
)

func requestInfos(clients []*lbclient.Client) []*lbrpc.InfoResponse {
	ch := make(chan *lbrpc.InfoResponse, len(clients))
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
	var infos []*lbrpc.InfoResponse
	for range clients {
		if info := <-ch; info != nil {
			infos = append(infos, info)
		}
	}
	sort.Slice(infos, func(i, j int) bool {
		// Names should be unique.
		// But this an awful place to rely on the assumption,
		// because if they are not subsequent runs of commands
		// will act on different daemons.
		// So play it safe.
		if infos[i].Name != infos[j].Name {
			return infos[i].Name < infos[j].Name
		}
		return infos[i].BossStart.Before(infos[j].BossStart)
	})
	return infos
}

func ls(args []string) {
	clients, err := lbclient.FindDaemons()
	if err != nil {
		fatalf("ls: %v\n", err)
	}
	infos := requestInfos(clients)

	for _, info := range infos {
		fmt.Printf("%s\n", info.Name)
	}
	os.Exit(0)
}
