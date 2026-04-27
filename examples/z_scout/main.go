// z_scout broadcasts SCOUT messages and prints every HELLO reply until the
// timeout elapses or the user interrupts.
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"

	"github.com/shirou/zenoh-go-client/zenoh"
)

func main() {
	timeoutMs := flag.Uint64("timeout", 1000, "scout timeout in milliseconds")
	flag.Parse()

	fmt.Println("Scouting...")
	hellos, err := zenoh.Scout(zenoh.NewConfig(),
		zenoh.NewFifoChannel[zenoh.Hello](16),
		&zenoh.ScoutOptions{TimeoutMs: *timeoutMs})
	if err != nil {
		log.Fatalf("scout: %v", err)
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt)
	fmt.Println("Press Ctrl-C to quit...")

	for {
		select {
		case <-stop:
			return
		case h, ok := <-hellos:
			if !ok {
				return
			}
			fmt.Println(h)
		}
	}
}
