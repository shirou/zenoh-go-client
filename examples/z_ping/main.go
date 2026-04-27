// z_ping measures round-trip latency by publishing on test/ping and
// reading the bounce-back on test/pong (companion: z_pong).
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
	size := flag.Int("size", 8, "payload size in bytes")
	samples := flag.Int("samples", 100, "number of measured pings")
	warmup := flag.Duration("warmup", time.Second, "warmup duration before measurement starts")
	noExpress := flag.Bool("no-express", false, "disable express batching")
	flag.Parse()

	openCtx, openCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer openCancel()
	session, err := zenoh.Open(openCtx, zenoh.NewConfig().WithEndpoint(*endpoint))
	if err != nil {
		log.Fatalf("open: %v", err)
	}
	defer session.Close()

	keyPing, _ := zenoh.NewKeyExpr("test/ping")
	keyPong, _ := zenoh.NewKeyExpr("test/pong")

	sub, err := session.DeclareSubscriber(keyPong, zenoh.NewFifoChannel[zenoh.Sample](16))
	if err != nil {
		log.Fatalf("declare subscriber: %v", err)
	}
	defer sub.Drop()
	recv, _ := sub.Handler().(<-chan zenoh.Sample)

	pub, err := session.DeclarePublisher(keyPing, &zenoh.PublisherOptions{
		CongestionControl: zenoh.CongestionControlBlock,
		HasCongestion:     true,
		IsExpress:         !*noExpress,
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

	fmt.Printf("Warming up for %v\n", *warmup)
	deadline := time.Now().Add(*warmup)
	for time.Now().Before(deadline) {
		if err := pub.Put(payload, nil); err != nil {
			log.Fatalf("put: %v", err)
		}
		<-recv
	}

	rtts := make([]int64, *samples)
	for i := 0; i < *samples; i++ {
		start := time.Now()
		if err := pub.Put(payload, nil); err != nil {
			log.Fatalf("put: %v", err)
		}
		<-recv
		rtts[i] = time.Since(start).Microseconds()
	}
	for i, rtt := range rtts {
		fmt.Printf("%d bytes: seq=%d rtt=%dus lat=%dus\n", *size, i, rtt, rtt/2)
	}
}
