// z_pong is the bounce-back side of z_ping: every sample on test/ping is
// re-published on test/pong with the same payload.
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
	noExpress := flag.Bool("no-express", false, "disable express batching")
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

	keyPing, _ := zenoh.NewKeyExpr("test/ping")
	keyPong, _ := zenoh.NewKeyExpr("test/pong")

	pub, err := session.DeclarePublisher(keyPong, &zenoh.PublisherOptions{
		CongestionControl: zenoh.CongestionControlBlock,
		HasCongestion:     true,
		IsExpress:         !*noExpress,
	})
	if err != nil {
		log.Fatalf("declare publisher: %v", err)
	}
	defer pub.Drop()

	sub, err := session.DeclareSubscriber(keyPing, zenoh.Closure[zenoh.Sample]{
		Call: func(s zenoh.Sample) { _ = pub.Put(s.Payload(), nil) },
	})
	if err != nil {
		log.Fatalf("declare subscriber: %v", err)
	}
	defer sub.Drop()

	fmt.Println("Pong ready — Ctrl-C to quit")
	<-ctx.Done()
}
