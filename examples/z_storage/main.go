// z_storage backs --key with an in-memory map: it records every PUT/DELETE
// it sees as a subscriber and replies to matching queries from its store.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"sync"
	"time"

	"github.com/shirou/zenoh-go-client/zenoh"
)

func main() {
	endpoint := flag.String("endpoint", "tcp/127.0.0.1:7447", "router endpoint")
	key := flag.String("key", "demo/example/**", "key expression to store")
	complete := flag.Bool("complete", false, "advertise the queryable as complete")
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

	var mu sync.Mutex
	store := make(map[string]zenoh.Sample)

	sub, err := session.DeclareSubscriber(ke, zenoh.Closure[zenoh.Sample]{
		Call: func(s zenoh.Sample) {
			fmt.Printf(">> [Subscriber] %s (%s: %q)\n", s.Kind(), s.KeyExpr(), s.Payload().String())
			mu.Lock()
			defer mu.Unlock()
			switch s.Kind() {
			case zenoh.SampleKindPut:
				store[s.KeyExpr().String()] = s
			case zenoh.SampleKindDelete:
				delete(store, s.KeyExpr().String())
			}
		},
	})
	if err != nil {
		log.Fatalf("declare subscriber: %v", err)
	}
	defer sub.Drop()

	qbl, err := session.DeclareQueryable(ke, func(q *zenoh.Query) {
		mu.Lock()
		defer mu.Unlock()
		for _, s := range store {
			if s.KeyExpr().Intersects(q.KeyExpr()) {
				if err := q.Reply(s.KeyExpr(), s.Payload(), nil); err != nil {
					log.Printf("reply: %v", err)
				}
			}
		}
	}, &zenoh.QueryableOptions{Complete: *complete})
	if err != nil {
		log.Fatalf("declare queryable: %v", err)
	}
	defer qbl.Drop()

	fmt.Printf("Storage on %s — Ctrl-C to quit\n", *key)
	<-ctx.Done()
}
