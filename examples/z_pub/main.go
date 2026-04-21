// z_pub publishes "[<N>] <value>" to --key every second until interrupted.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"time"

	"github.com/shirou/zenoh-go-client/zenoh"
)

func main() {
	endpoint := flag.String("endpoint", "tcp/127.0.0.1:7447", "router endpoint")
	key := flag.String("key", "demo/example/zenoh-go-pub", "key expression")
	value := flag.String("value", "Pub from Zenoh-Go-Client!", "payload")
	flag.Parse()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	go func() { <-sig; cancel() }()

	openCtx, openCancel := context.WithTimeout(ctx, 5*time.Second)
	defer openCancel()
	session, err := zenoh.Open(openCtx, zenoh.NewConfig().WithEndpoint(*endpoint))
	if err != nil {
		log.Fatalf("open: %v", err)
	}
	defer session.Close()

	ke, err := zenoh.NewKeyExpr(*key)
	if err != nil {
		log.Fatalf("key: %v", err)
	}
	pub, err := session.DeclarePublisher(ke, nil)
	if err != nil {
		log.Fatalf("declare publisher: %v", err)
	}
	defer pub.Drop()

	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for i := 0; ; i++ {
		payload := fmt.Sprintf("[%4d] %s", i, *value)
		fmt.Println("Putting:", payload)
		if err := pub.Put(zenoh.NewZBytesFromString(payload), nil); err != nil {
			log.Printf("put: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}
