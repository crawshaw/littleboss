package main

import (
	"reflect"
	"testing"
)

const psText = `45698 vi ltboss.go
12960 -bash
12345 /path/to/ltboss -daemon -name=web-server-1 -socketpath=/tmp/ltboss-23983/ltboss-socket.12345
54321 /path/to/ltboss -daemon -name=mail-server-1 -socketpath=/tmp/ltboss-23987/ltboss-socket.54321
46921 man ssh-agent
46922 sh -c (cd '/usr/share/man' && (echo ".ll 11.3i"; echo ".nr LL 11.3i"; /bin/cat '/usr/share/man/man1/ssh-agent.1') | /usr/bin/tbl | /usr/bin/groff -Wall -mtty-char -Tascii
`

func TestParsePS(t *testing.T) {
	const cmdpath = `/path/to/ltboss`
	got, err := parsePS(cmdpath, []byte(psText))
	if err != nil {
		t.Fatal(err)
	}

	want := []DaemonInfo{
		{PID: 12345, ServiceName: "web-server-1", SocketPath: "/tmp/ltboss-23983/ltboss-socket.12345"},
		{PID: 54321, ServiceName: "mail-server-1", SocketPath: "/tmp/ltboss-23987/ltboss-socket.54321"},
	}

	if !reflect.DeepEqual(want, got) {
		t.Errorf("got: %#+v\nwant: %#+v", got, want)
	}
}
