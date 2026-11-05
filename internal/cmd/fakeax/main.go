// Command fakeax serves an in-memory AX control plane with sample resources,
// for trying ax-cli without a cluster:
//
//	go run ./internal/cmd/fakeax &
//	AX_SERVER=http://127.0.0.1:8080 go run .
package main

import (
	"context"
	_ "embed"
	"flag"
	"log"
	"os"
	"os/signal"

	"github.com/google/ax-cli/internal/ax"
	"github.com/google/ax-cli/internal/fakeax"
)

//go:embed seed.yaml
var seed []byte

func main() {
	addr := flag.String("addr", "127.0.0.1:8080", "listen address")
	empty := flag.Bool("empty", false, "start without sample resources")
	flag.Parse()

	bound, stop, err := fakeax.New().Serve(*addr)
	if err != nil {
		log.Fatal(err)
	}
	defer stop()

	if !*empty {
		if err := load(bound); err != nil {
			log.Fatal(err)
		}
	}
	log.Printf("fake AX control plane on http://%s (AX_SERVER=http://%s)", bound, bound)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	<-ctx.Done()
}

// load applies the sample manifest and starts some of its tasks.
func load(addr string) error {
	c, err := ax.Dial(ax.Target{Server: "http://" + addr, Namespace: "ax-system"}, "ax")
	if err != nil {
		return err
	}
	defer c.Close()
	ctx := context.Background()
	if _, err := c.Apply(ctx, seed, ""); err != nil {
		return err
	}
	for _, name := range []string{"api-refactor", "flaky-tests"} {
		if _, err := c.Resume(ctx, "default", name); err != nil {
			return err
		}
	}
	return nil
}
