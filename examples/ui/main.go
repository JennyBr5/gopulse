package main

import (
	"context"
	"fmt"
	"time"

	"github.com/cploutarchou/gopulse/trace"
)

func main() {
	// Start tracing to stdout so we can also redirect to a file if needed
	_ = trace.Start(trace.Config{Output: "stdout"})
	defer trace.Stop()

	// Start the in-app live UI
	stop, _ := trace.UI(":8080")
	defer stop(context.Background())

	ch := trace.NewChan[int](0, "demo-ui")

	trace.Go(func() {
		defer ch.Close()
		for i := 0; i < 1000; i++ {
			ch.Send(i)
			time.Sleep(5 * time.Millisecond)
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

	// Simulate a blocking point in main
	unblock := trace.Block("startup work")
	time.Sleep(100 * time.Millisecond)
	unblock()

	fmt.Println("ui example running; open http://localhost:8080")
	// Keep process alive for viewing UI
	time.Sleep(60 * time.Second)
}
