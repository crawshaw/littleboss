package littleboss_test

import (
	"bufio"
	"bytes"
	"go/build"
	"io"
	"io/ioutil"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	_ "crawshaw.io/littleboss"
)

const helloProgram = `package main

import (
	"context"

	"crawshaw.io/littleboss"
)

func main() {
	lb := littleboss.New("hello_program", nil)
	lb.Run(func(context.Context) { println("hello, from littleboss.") })
}
`

func TestBypass(t *testing.T) {
	goTool := findGoTool(t)

	dir, err := ioutil.TempDir("", "littleboss_test_")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	helloPath := filepath.Join(dir, "hello.go")
	if err := ioutil.WriteFile(helloPath, []byte(helloProgram), 0666); err != nil {
		t.Fatal(err)
	}

	output, err := exec.Command(goTool, "run", helloPath).CombinedOutput()
	if err != nil {
		t.Fatalf("go run hello.go: %v: %s", err, output)
	}
	if got, want := string(output), "hello, from littleboss.\n"; got != want {
		t.Errorf("go run hello.go = %q, want %q", got, want)
	}

	output, err = exec.Command(goTool, "run", helloPath, "-littleboss=bypass").CombinedOutput()
	if err != nil {
		t.Fatalf("go run hello.go -littleboss=bypass: %v: %s", err, output)
	}
	if got, want := string(output), "hello, from littleboss.\n"; got != want {
		t.Errorf("go run hello.go -littleboss=bypass = %q, want %q", got, want)
	}
}

const echoServer = `package main

import (
	"bufio"
	"context"
	"fmt"
	"os"

	"crawshaw.io/littleboss"
)

func main() {
	lb := littleboss.New("echo_server", nil)
	flagAddr := lb.Listener("addr", "tcp", ":0", "addr to dial to hear lines echoed")
	lb.Run(func(ctx context.Context) {
		ln := flagAddr.Listener()
		fmt.Println(ln.Addr())
		defer func() {
			<-ctx.Done()
			fmt.Println("entering lameduck mode")
			ln.Close()
		}()
		for {
			conn, err := ln.Accept()
			if err != nil {
				ln.Close()
				os.Exit(0)
			}
			br := bufio.NewReader(conn)
			str, err := br.ReadBytes('\n')
			if err != nil {
				conn.Close()
				fmt.Printf("conn read bytes failed: %v\n", err)
				continue
			}
			conn.Write(str)
			conn.Close()
		}
	})
}
`

func TestStartStopReload(t *testing.T) {
	goTool := findGoTool(t)

	dir, err := ioutil.TempDir("", "littleboss_test_")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	os.MkdirAll(filepath.Join(dir, "src", "echo_server"), 0777)
	srcPath := filepath.Join(dir, "src", "echo_server", "echo_server.go")
	if err := ioutil.WriteFile(srcPath, []byte(echoServer), 0666); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(goTool, "install", "echo_server")
	cmd.Env = append(os.Environ(), "GOPATH="+dir+":"+build.Default.GOPATH)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go install echo_server: %v: %s", err, output)
	}

	echoPath := filepath.Join(dir, "bin", "echo_server")
	buf := new(bytes.Buffer)
	cmd = exec.Command(echoPath, "-littleboss=start")
	cmd.Stdout = buf
	cmd.Stderr = buf
	if err := cmd.Start(); err != nil {
		t.Fatalf("./bin/echo_server -littleboss=start: %v: %s", err, buf.Bytes())
	}
	defer cmd.Process.Kill()
	time.Sleep(50 * time.Millisecond)
	addr, err := buf.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("echo_server address: %q", addr)
	port := addr[strings.LastIndex(addr, ":")+1 : len(addr)-1]

	const want = "hello\n"
	conn, err := net.Dial("tcp", net.JoinHostPort("localhost", port))
	if err != nil {
		t.Fatalf("could not dial echo server: %v", err)
	}
	if _, err := io.WriteString(conn, want); err != nil {
		t.Fatalf("could not write to echo server: %v", err)
	}
	br := bufio.NewReader(conn)
	got, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("could not read from echo server: %v", err)
	}
	conn.Close()
	if got != want {
		t.Errorf("echo server replied with %q, want %q", got, want)
	}

	if output, err := exec.Command(echoPath, "-littleboss=reload").CombinedOutput(); err != nil {
		t.Fatalf("reload failed: %v: %s\necho_server output:\n%s", err, output, buf.Bytes())
	} else {
		if s := buf.String(); !strings.Contains(s, "reload requested") {
			t.Errorf("echo_server does not mention reload in stdout:\n%s", s)
		}
	}

	/*buf.Truncate(0)

	output, err := exec.Command(echoPath, "-littleboss=stop").CombinedOutput()
	if err != nil {
		t.Fatalf("stop failed: %v: %s\necho_server output:\n%s", err, output, buf.Bytes())
	}
	cmd.Wait()
	if s := buf.String(); !strings.Contains(s, "lameduck mode") {
		t.Errorf("echo_server does not mention lameduck mode:\n%s", s)
	}*/
}

func findGoTool(t *testing.T) string {
	path := filepath.Join(runtime.GOROOT(), "bin", "go")
	if err := exec.Command(path, "version").Run(); err == nil {
		return path
	}
	path, err := exec.LookPath("go")
	if err2 := exec.Command(path, "version").Run(); err == nil && err2 == nil {
		if err == nil {
			err = err2
		}
		t.Fatalf("go tool is not available: %v", err2)
	}
	return path
}
