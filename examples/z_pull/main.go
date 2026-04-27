// z_pull declares a ring-buffered subscriber and pulls one sample every
// --interval, simulating a slow consumer that drops the oldest pending
// samples when the buffer fills.
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
	size := flag.Int("size", 3, "ring-buffer capacity")
	interval := flag.Duration("interval", 5*time.Second, "delay between pulls")
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
	sub, err := session.DeclareSubscriber(ke, zenoh.NewRingChannel[zenoh.Sample](*size))
	if err != nil {
		log.Fatalf("declare subscriber: %v", err)
	}
	defer sub.Drop()

	samples, _ := sub.Handler().(<-chan zenoh.Sample)
	if samples == nil {
		log.Fatal("ring channel handle is not <-chan Sample")
	}

	fmt.Printf("Pulling from %s every %v — Ctrl-C to quit\n", *key, *interval)
	for {
		select {
		case <-ctx.Done():
			return
		case s, ok := <-samples:
			if !ok {
				return
			}
			fmt.Printf(">> [Pull] %s %s %q — sleeping %v\n",
				s.Kind(), s.KeyExpr(), s.Payload().String(), *interval)
			select {
			case <-ctx.Done():
				return
			case <-time.After(*interval):
			}
		}
	}
}
