// z_put publishes a single PUT to --key with --value, then exits.
package main

import (
	"context"
	"flag"
	"log"
	"time"

	"github.com/shirou/zenoh-go-client/zenoh"
)

func main() {
	endpoint := flag.String("endpoint", "tcp/127.0.0.1:7447", "router endpoint")
	key := flag.String("key", "demo/example/zenoh-go-put", "key expression")
	value := flag.String("value", "Put from Zenoh-Go-Client!", "payload")
	flag.Parse()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	session, err := zenoh.Open(ctx, zenoh.NewConfig().WithEndpoint(*endpoint))
	if err != nil {
		log.Fatalf("open: %v", err)
	}
	defer session.Close()

	ke, err := zenoh.NewKeyExpr(*key)
	if err != nil {
		log.Fatalf("key: %v", err)
	}
	if err := session.Put(ke, zenoh.NewZBytesFromString(*value), nil); err != nil {
		log.Fatalf("put: %v", err)
	}
}
