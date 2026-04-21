// z_queryable declares a queryable that replies with --value for every incoming query.
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
	key := flag.String("key", "demo/example/zenoh-go-queryable", "key expression")
	value := flag.String("value", "Queryable from Zenoh-Go-Client!", "reply payload")
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
	qbl, err := session.DeclareQueryable(ke, func(q *zenoh.Query) {
		fmt.Printf(">> [Queryable] %s (%s)\n", q.KeyExpr(), q.Parameters())
		if err := q.Reply(ke, zenoh.NewZBytesFromString(*value), nil); err != nil {
			log.Printf("reply: %v", err)
		}
	}, nil)
	if err != nil {
		log.Fatalf("declare queryable: %v", err)
	}
	defer qbl.Drop()

	fmt.Println("Queryable on", *key, "— Ctrl-C to quit")
	<-ctx.Done()
}
