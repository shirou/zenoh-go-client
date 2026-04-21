// z_get issues one Get on --key and prints every Reply until RESPONSE_FINAL.
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
	key := flag.String("key", "demo/example/**", "key expression")
	timeout := flag.Duration("timeout", 10*time.Second, "query timeout")
	flag.Parse()

	ctx, cancel := context.WithTimeout(context.Background(), *timeout+5*time.Second)
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
	replies, err := session.Get(ke, &zenoh.GetOptions{Timeout: *timeout})
	if err != nil {
		log.Fatalf("get: %v", err)
	}
	for r := range replies {
		if s, ok := r.Sample(); ok {
			fmt.Printf(">> [Get] ok %s %q\n", s.KeyExpr(), s.Payload().String())
		} else if p, _, ok := r.Err(); ok {
			fmt.Printf(">> [Get] err %q\n", p.String())
		}
	}
}
