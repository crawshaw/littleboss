# littleboss: self-supervising Go binaries

A Go package, littleboss lets you turn your program into a
a self-supervising binary.
It starts itself as a child process, monitors its life cycle,
reloads it if it exits, and can be instructed to replace it with
a new binary.

The supervisor can open sockets for you and share them across
reloads of your program, ensuring no connections are dropped.

You can install it with:

```
go get crawshaw.io/littleboss
```

Make a program use littleboss by modifying the main function:

```go
func main() {
	lb := littleboss.New("service-name")
	lb.Run(func(ctx context.Context) {
		// main goes here, exit when <-ctx.Done()
	})
}
```

The service name is used to identify which program the supervisor will control.

## Usage

By default the supervisor is bypassed and the program executes directly.
A flag, -littleboss, is added to the binary.
It can be used to start a supervised binary and manage it:

```
$ mybin &                     # binary runs directly, no child process
$ mybin -littleboss=start &   # supervisor is created
$ mybin2 -littleboss=reload   # child is replaced by new mybin2 process
$ mybin -littleboss=stop      # supervisor and child are shut down
```

## Configuration

Supervisor options are baked into the binary.
The littleboss struct type contains fields that can be set before calling
the Run method to configure the supervisor.
Options include reloading the previous binary if a reload fails,
controlling how long an exiting program has to turn down its connections,
and specifying exactly what flags control and are passed by littleboss.

## An HTTP server example

```go
func main() {
	lb := littleboss.New("myblog")
	flagHTTPS := lb.Listener("https", "tcp", ":443", "address")
	lb.Run(func(ctx context.Context) {
		httpMain(ctx, flagHTTPS.Listener())
	})
}

func httpMain(ctx context.Context, ln net.Listener) {
	srv := &http.Server{
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
		IdleTimeout:  60 * time.Second,
		Handler:      blogHandler,
	}
	go func() {
		if err := srv.ServeTLS(ln, "certfile", "keyfile"); err != nil {
			if err == http.ErrServerClosed {
				return
			}
			log.Fatal(err)
		}
	}()

	<-ctx.Done()
	srv.Shutdown(ctx)
}
```
