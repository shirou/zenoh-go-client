// z_get_liveliness queries currently-alive liveliness tokens that match
// --key and prints each one until RESPONSE_FINAL.
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
	key := flag.String("key", "group1/**", "liveliness key expression")
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
	fmt.Printf("Sending liveliness query %q...\n", *key)
	replies, err := session.Liveliness().Get(ke, &zenoh.LivelinessGetOptions{Timeout: *timeout})
	if err != nil {
		log.Fatalf("get: %v", err)
	}
	for r := range replies {
		if s, ok := r.Sample(); ok {
			fmt.Printf(">> Alive token (%s)\n", s.KeyExpr())
		} else if p, _, ok := r.Err(); ok {
			fmt.Printf(">> err %q\n", p.String())
		}
	}
}
