// z_pub_thr publishes a fixed payload on test/thr as fast as it can,
// for use with z_sub_thr to measure throughput.
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
	size := flag.Int("size", 8, "payload size in bytes")
	priority := flag.Uint("priority", uint(zenoh.PriorityDefault),
		fmt.Sprintf("priority [%d-%d]", zenoh.PriorityRealTime, zenoh.PriorityBackground))
	express := flag.Bool("express", false, "enable express batching")
	flag.Parse()

	prio := zenoh.Priority(*priority)
	if !prio.IsValid() {
		log.Fatalf("invalid priority %d", *priority)
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

	ke, _ := zenoh.NewKeyExpr("test/thr")
	pub, err := session.DeclarePublisher(ke, &zenoh.PublisherOptions{
		Priority:    prio,
		HasPriority: true,
		IsExpress:   *express,
	})
	if err != nil {
		log.Fatalf("declare publisher: %v", err)
	}
	defer pub.Drop()

	data := make([]byte, *size)
	for i := range data {
		data[i] = byte(i % 10)
	}
	payload := zenoh.NewZBytes(data)

	fmt.Println("Publishing on test/thr — Ctrl-C to quit")
	for {
		select {
		case <-ctx.Done():
			return
		default:
			if err := pub.Put(payload, nil); err != nil {
				// During reconnect the publisher may briefly refuse puts;
				// pause a tick so we don't tight-loop on errors.
				time.Sleep(time.Millisecond)
			}
		}
	}
}
