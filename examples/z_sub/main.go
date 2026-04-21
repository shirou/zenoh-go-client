// z_sub subscribes to --key and prints every sample until interrupted.
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
	key := flag.String("key", "demo/example/**", "key expression")
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
	sub, err := session.DeclareSubscriber(ke, zenoh.Closure[zenoh.Sample]{
		Call: func(s zenoh.Sample) {
			fmt.Printf(">> [Subscriber] %s %q\n", s.KeyExpr(), s.Payload().String())
		},
	})
	if err != nil {
		log.Fatalf("declare subscriber: %v", err)
	}
	defer sub.Drop()

	fmt.Println("Subscribed to", *key, "— Ctrl-C to quit")
	<-ctx.Done()
}
