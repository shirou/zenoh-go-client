// z_querier declares a querier on --key and issues a Get every second
// until interrupted, printing each Reply.
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

func parseTarget(s string) (zenoh.QueryTarget, error) {
	switch s {
	case "BEST_MATCHING":
		return zenoh.QueryTargetBestMatching, nil
	case "ALL":
		return zenoh.QueryTargetAll, nil
	case "ALL_COMPLETE":
		return zenoh.QueryTargetAllComplete, nil
	default:
		return 0, fmt.Errorf("unsupported query target %q", s)
	}
}

func main() {
	endpoint := flag.String("endpoint", "tcp/127.0.0.1:7447", "router endpoint")
	key := flag.String("key", "demo/example/**", "key expression")
	parameters := flag.String("parameters", "", "optional query parameters")
	payload := flag.String("payload", "", "optional payload to include in each query")
	targetStr := flag.String("target", "BEST_MATCHING", "query target (BEST_MATCHING | ALL | ALL_COMPLETE)")
	timeout := flag.Duration("timeout", 10*time.Second, "per-query timeout")
	flag.Parse()

	target, err := parseTarget(*targetStr)
	if err != nil {
		log.Fatal(err)
	}

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
	querier, err := session.DeclareQuerier(ke, &zenoh.QuerierOptions{
		Target:    target,
		HasTarget: true,
		Timeout:   *timeout,
	})
	if err != nil {
		log.Fatalf("declare querier: %v", err)
	}
	defer querier.Drop()

	fmt.Printf("Querying %s every second — Ctrl-C to quit\n", *key)
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for i := 0; ; i++ {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		msg := fmt.Sprintf("[%4d] %s", i, *payload)
		fmt.Printf("Querying %s with payload %q...\n", *key, msg)
		replies, err := querier.Get(&zenoh.QuerierGetOptions{Parameters: *parameters})
		if err != nil {
			log.Printf("get: %v", err)
			continue
		}
		for r := range replies {
			if s, ok := r.Sample(); ok {
				fmt.Printf(">> Received (%s: %q)\n", s.KeyExpr(), s.Payload().String())
			} else if p, _, ok := r.Err(); ok {
				fmt.Printf(">> Received (ERROR: %q)\n", p.String())
			}
		}
	}
}
