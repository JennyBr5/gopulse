package main

import (
	"fmt"
	"time"

	"github.com/cploutarchou/gopulse/trace"
)

// This example emits events to stdout; redirect to a file and use the CLI to analyze/visualize.
//
//	go run ./examples/cli > events.log
//	gopulse analyze events.log
//	gopulse web -addr :8080 -live events.log
func main() {
	_ = trace.Start(trace.Config{Output: "stdout"})
	defer trace.Stop()

	ch := trace.NewChan[int](0, "demo-cli")

	trace.Go(func() {
		defer ch.Close()
		for i := 0; i < 50; i++ {
			ch.Send(i)
			time.Sleep(10 * time.Millisecond)
		}
	})

	trace.Go(func() {
		for {
			v, ok := ch.Recv()
			if !ok {
				break
			}
			_ = v
		}
	})

	// Brief block in main
	unblock := trace.Block("initialization")
	time.Sleep(50 * time.Millisecond)
	unblock()

	// Give workers time to finish; then exit (no long sleep)
	time.Sleep(500 * time.Millisecond)
	fmt.Println("cli example done")
}
