// z_sub_liveliness subscribes to liveliness changes on --key. Put samples
// signal a new alive token; Delete samples signal a dropped token.
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
	key := flag.String("key", "group1/**", "liveliness key expression")
	history := flag.Bool("history", false, "deliver currently-alive tokens at declare time")
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
	sub, err := session.Liveliness().DeclareSubscriber(ke, zenoh.Closure[zenoh.Sample]{
		Call: func(s zenoh.Sample) {
			switch s.Kind() {
			case zenoh.SampleKindPut:
				fmt.Printf(">> [LivelinessSubscriber] New alive token (%s)\n", s.KeyExpr())
			case zenoh.SampleKindDelete:
				fmt.Printf(">> [LivelinessSubscriber] Dropped token (%s)\n", s.KeyExpr())
			}
		},
	}, &zenoh.LivelinessSubscriberOptions{History: *history})
	if err != nil {
		log.Fatalf("declare liveliness subscriber: %v", err)
	}
	defer sub.Drop()

	fmt.Printf("Liveliness subscriber on %s — Ctrl-C to quit\n", *key)
	<-ctx.Done()
}
