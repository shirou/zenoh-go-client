// z_info opens a session and prints the local ZenohID plus the IDs of
// every currently-visible router and peer.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"time"

	"github.com/shirou/zenoh-go-client/zenoh"
)

func main() {
	endpoint := flag.String("endpoint", "tcp/127.0.0.1:7447", "router endpoint")
	flag.Parse()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	session, err := zenoh.Open(ctx, zenoh.NewConfig().WithEndpoint(*endpoint))
	if err != nil {
		log.Fatalf("open: %v", err)
	}
	defer session.Close()

	fmt.Printf("own id: %s\n", session.ZId())

	fmt.Println("routers ids:")
	for _, id := range session.RoutersZId() {
		fmt.Printf("  %s\n", id)
	}

	fmt.Println("peers ids:")
	for _, id := range session.PeersZId() {
		fmt.Printf("  %s\n", id)
	}
}
