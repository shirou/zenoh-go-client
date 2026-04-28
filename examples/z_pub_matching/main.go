// z_pub_matching publishes "[<N>] <value>" to --key once per second and
// prints every MatchingListener transition (true/false) emitted by the
// router. Use it alongside z_sub on the same key to watch the matching
// status flip as subscribers come and go.
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

	// Channel-backed listener so we can range over it from a watcher
	// goroutine. Buffer is small — the listener never bursts: at most
	// one delivery per remote D_/U_SUBSCRIBER transition.
	ml, err := pub.DeclareMatchingListener(zenoh.NewFifoChannel[zenoh.MatchingStatus](4))
	if err != nil {
		log.Fatalf("declare matching listener: %v", err)
	}
	defer ml.Drop()

	go func() {
		for st := range ml.Handler() {
			if st.Matching {
				fmt.Println("[Matching] Publisher has matching subscribers.")
			} else {
				fmt.Println("[Matching] Publisher has NO MORE matching subscribers.")
			}
		}
	}()

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
