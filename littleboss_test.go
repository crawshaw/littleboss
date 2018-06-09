// Copyright (c) 2018 David Crawshaw <david@zentus.com>
//
// Permission to use, copy, modify, and distribute this software for any
// purpose with or without fee is hereby granted, provided that the above
// copyright notice and this permission notice appear in all copies.
//
// THE SOFTWARE IS PROVIDED "AS IS" AND THE AUTHOR DISCLAIMS ALL WARRANTIES
// WITH REGARD TO THIS SOFTWARE INCLUDING ALL IMPLIED WARRANTIES OF
// MERCHANTABILITY AND FITNESS. IN NO EVENT SHALL THE AUTHOR BE LIABLE FOR
// ANY SPECIAL, DIRECT, INDIRECT, OR CONSEQUENTIAL DAMAGES OR ANY DAMAGES
// WHATSOEVER RESULTING FROM LOSS OF USE, DATA OR PROFITS, WHETHER IN AN
// ACTION OF CONTRACT, NEGLIGENCE OR OTHER TORTIOUS ACTION, ARISING OUT OF
// OR IN CONNECTION WITH THE USE OR PERFORMANCE OF THIS SOFTWARE.

package littleboss_test

import (
	"bufio"
	"bytes"
	"fmt"
	"go/build"
	"io"
	"io/ioutil"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	_ "crawshaw.io/littleboss"
	_ "github.com/spf13/pflag"
)

const helloProgram = `package main

import (
	"context"
	"time"

	"crawshaw.io/littleboss"
)

func main() {
	lb := littleboss.New("hello_program")
	lb.SupervisorInit = func() { panic("not called in bypass") }
	flagAddr := lb.Listener("addr", "tcp", ":0", "")
	lb.Run(func(context.Context) {
		ln := flagAddr.Listener()
		go func() {
			time.Sleep(10*time.Millisecond)
			ln.Close()
		}()
		// ln.Accept releases on ln.Close, unless an *os.File has been
		// extracted from the listener. This should not happen in
		// bypass mode.
		ln.Accept()
		println("hello, from littleboss.")
	})
}
`

func TestBypass(t *testing.T) {
	helloPath := goBuild(t, "hello", helloProgram)
	output, err := exec.Command(helloPath).CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %s", err, output)
	}
	if got, want := string(output), "hello, from littleboss.\n"; got != want {
		t.Errorf("output = %q, want %q", got, want)
	}

	output, err = exec.Command(helloPath, "-littleboss=bypass").CombinedOutput()
	if err != nil {
		t.Fatalf("hello -littleboss=bypass: %v: %s", err, output)
	}
	if got, want := string(output), "hello, from littleboss.\n"; got != want {
		t.Errorf("hello -littleboss=bypass = %q, want %q", got, want)
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
	lb := littleboss.New("echo_server")
	lb.SupervisorInit = func() { fmt.Println("SupervisorInit called") }
	flagAddr := lb.Listener("addr", "tcp", ":0", "addr to dial to hear lines echoed")
	lb.Run(func(ctx context.Context) {
		ln := flagAddr.Listener()
		fmt.Printf("addr=%s\n", ln.Addr())
		go func() {
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
	echoPath := goBuild(t, "echo_server", echoServer)
	buf := new(bytes.Buffer)
	cmd := exec.Command(echoPath, "-littleboss=start")
	cmd.Stdout = buf
	cmd.Stderr = buf
	if err := cmd.Start(); err != nil {
		t.Fatalf("./bin/echo_server -littleboss=start: %v: %s", err, buf.Bytes())
	}
	defer cmd.Process.Kill()
	time.Sleep(50 * time.Millisecond)
	var addr string
	if str, i := buf.String(), strings.Index(buf.String(), "addr="); i >= 0 {
		addr = str[i:]
		if i := strings.Index(addr, "\n"); i > 0 {
			addr = addr[:i]
		}
	} else {
		t.Fatalf("no addr in output:\n%s", str)
	}
	t.Logf("echo_server address: %q", addr)
	port := addr[strings.LastIndex(addr, ":")+1:]

	const want = "hello\n"
	conn, err := net.Dial("tcp", net.JoinHostPort("localhost", port))
	if err != nil {
		t.Logf("echo_sever output:\n%s", buf.String())
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

	output, err := exec.Command(echoPath, "-littleboss=stop").CombinedOutput()
	if err != nil {
		t.Fatalf("stop failed: %v: %s\necho_server output:\n%s", err, output, buf.Bytes())
	}
	cmd.Wait()

	if s := buf.String(); strings.Count(s, "lameduck mode") != 2 {
		// once for reload, and once for stop
		t.Errorf("echo_server does not mention lameduck mode:\n%s", s)
	}
	if count := strings.Count(buf.String(), "SupervisorInit called"); count != 1 {
		t.Errorf("echo_server called SupervisorInit %d times, want 1", count)
	}
}

const blockerServer = `package main

import (
	"context"
	"fmt"
	"time"

	"crawshaw.io/littleboss"
)

func main() {
	lb := littleboss.New("blocker")
	lb.LameduckTimeout = 250*time.Millisecond
	lb.Run(func(ctx context.Context) {
		fmt.Println("started")
		<-ctx.Done()
		fmt.Println("got lameduck signal, blocking")
		select {}
	})
}
`

func TestBlockingSIGINT(t *testing.T) {
	path := goBuild(t, "blocker", blockerServer)
	buf := new(bytes.Buffer)
	cmd := exec.Command(path, "-littleboss=start")
	cmd.Stdout = buf
	cmd.Stderr = buf
	if err := cmd.Start(); err != nil {
		t.Fatalf("-littleboss=start: %v: %s", err, buf.Bytes())
	}
	for {
		if buf.Len() > 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	cmd.Process.Signal(syscall.SIGINT)
	cmd.Wait()

	out := buf.String()
	if !strings.Contains(out, "got lameduck signal, blocking") {
		t.Errorf("output does not mention lameduck mode:\n%s", out)
	}
}

const pflagSrc = `package main

import (
	"context"
	"flag"
	"fmt"

	"crawshaw.io/littleboss"
	"github.com/spf13/pflag"
)

func main() {
	lb := littleboss.New("pflag")
	lb.Persist = false
	lb.Command("-mylb", pflag.String("mylb", "start", ""))
	pflag.CommandLine.AddGoFlagSet(flag.CommandLine)
	pflag.Parse()
	lb.Run(func(ctx context.Context) {
		fmt.Println("running pflag")
	})
}
`

func TestPFlag(t *testing.T) {
	path := goBuild(t, "pflag", pflagSrc)
	buf := new(bytes.Buffer)
	cmd := exec.Command(path, "--mylb=start")
	cmd.Stdout = buf
	cmd.Stderr = buf
	if err := cmd.Start(); err != nil {
		t.Fatalf("%v: %s", err, buf.Bytes())
	}
	cmd.Wait()

	out := buf.String()
	if !strings.Contains(out, "running pflag") {
		t.Errorf("output does not mention pflag:\n%s", out)
	}
}

const failureServer = `package main

import (
	"context"
	"fmt"
	"os"

	"crawshaw.io/littleboss"
)

func main() {
	const version = %d
	lb := littleboss.New("blocker")
	lb.Run(func(ctx context.Context) {
		if version > 1 {
			fmt.Println("failed to start version", version)
			os.Exit(1)
		}
		fmt.Println("running version", version)
		<-ctx.Done()
	})
}
`

func TestReloadFailure(t *testing.T) {
	failure1 := goBuild(t, "failure1", fmt.Sprintf(failureServer, 1))
	buf := new(bytes.Buffer)
	cmd := exec.Command(failure1, "-littleboss=start")
	cmd.Stdout = buf
	cmd.Stderr = buf
	if err := cmd.Start(); err != nil {
		t.Fatalf("-littleboss=start: %v: %s", err, buf.Bytes())
	}
	for {
		if buf.Len() > 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	out := buf.String()
	if !strings.Contains(out, "running version 1") {
		t.Errorf("output does not mention version 1:\n%s", out)
	}

	failure2 := goBuild(t, "failure2", fmt.Sprintf(failureServer, 2))
	buf2 := new(bytes.Buffer)
	cmd2 := exec.Command(failure2, "-littleboss=reload")
	cmd2.Stdout = buf2
	cmd2.Stderr = buf2
	if err := cmd2.Run(); err == nil {
		t.Fatalf("expected failure2 to fail, but it didn't:\n%s\n\nboss output:\n%s", buf2.String(), buf.String())
	}

	for i := 0; i < 10; i++ {
		if count := strings.Count(buf.String(), "running version 1"); count > 1 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	if output, err := exec.Command(failure1, "-littleboss=stop").CombinedOutput(); err != nil {
		t.Fatalf("stop failed:\n%s", output)
	}

	out = buf.String()
	if got := strings.Count(out, "running version 1"); got != 2 {
		t.Fatalf("expected two v1 starts, got %d. output:\n%s", got, out)
	}
}

var tempdir string

func TestMain(m *testing.M) {
	var err error
	tempdir, err = ioutil.TempDir("", "littleboss_test_")
	if err != nil {
		fmt.Fprintf(os.Stderr, "TestMain: %v\n", err)
		os.Exit(1)
	}
	exitCode := m.Run()
	os.RemoveAll(tempdir)
	os.Exit(exitCode)
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

func goBuild(t *testing.T, name, src string) (path string) {
	goTool := findGoTool(t)

	os.MkdirAll(filepath.Join(tempdir, "bin"), 0777)
	os.MkdirAll(filepath.Join(tempdir, "src", name), 0777)
	srcPath := filepath.Join(tempdir, "src", name, name+".go")
	if err := ioutil.WriteFile(srcPath, []byte(src), 0666); err != nil {
		t.Fatalf("writing %s: %v", name, err)
	}

	cmd := exec.Command(goTool, "install", name)
	cmd.Env = os.Environ()
	cmd.Env = append(cmd.Env, "GOPATH="+tempdir+":"+build.Default.GOPATH)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go install %s: %v: %s", name, err, output)
	}

	return filepath.Join(tempdir, "bin", name)
}
