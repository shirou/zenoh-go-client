// z_sub_thr counts samples on test/thr and prints msgs/s every
// --number messages, exiting after --samples rounds.
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

type stats struct {
	count            uint64
	finishedRounds   uint64
	start            time.Time
	maxRounds        uint64
	messagesPerRound uint64
	done             chan struct{}
	closed           bool
}

func (s *stats) update() {
	switch {
	case s.count == 0:
		s.start = time.Now()
		s.count++
	case s.count < s.messagesPerRound:
		s.count++
	default:
		s.finishedRounds++
		elapsed := time.Since(s.start).Microseconds()
		rate := float64(s.messagesPerRound) * 1_000_000.0 / float64(elapsed)
		fmt.Printf("%v msgs/s\n", rate)
		s.count = 0
		if s.finishedRounds > s.maxRounds && !s.closed {
			s.closed = true
			close(s.done)
		}
	}
}

func main() {
	endpoint := flag.String("endpoint", "tcp/127.0.0.1:7447", "router endpoint")
	samples := flag.Uint64("samples", 10, "number of measurements")
	number := flag.Uint64("number", 1_000_000, "messages per measurement")
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

	ke, _ := zenoh.NewKeyExpr("test/thr")
	st := &stats{maxRounds: *samples, messagesPerRound: *number, done: make(chan struct{})}
	sub, err := session.DeclareSubscriber(ke, zenoh.Closure[zenoh.Sample]{
		Call: func(zenoh.Sample) { st.update() },
	})
	if err != nil {
		log.Fatalf("declare subscriber: %v", err)
	}
	defer sub.Drop()

	fmt.Println("Counting samples on test/thr — Ctrl-C to quit")
	select {
	case <-ctx.Done():
	case <-st.done:
	}
}
