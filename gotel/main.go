package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"gotel/probe"
)

func main() {
	urls := []string{
		"https://startly-api.onrender.com/health",
		"https://does-not-exist.invalid/",
		"https://httpbin.org/status/500",
	}
	p := probe.New(probe.DefaultConfig())
	policy := probe.Default2xx

	failed := false
	for _, url := range urls {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		r := p.Probe(ctx, url)
		cancel()

		if !policy.Healthy(r) {
			failed = true
			if r.Err != nil {
				fmt.Fprintf(os.Stderr, "FAIL %s after %v: %v\n", r.URL, r.Total, r.Err)
			} else {
				fmt.Fprintf(os.Stderr, "FAIL %s status=%d (not allowed by policy) total=%v\n", r.URL, r.StatusCode, r.Total)
			}
			continue
		}

		fmt.Printf("OK   %s status=%d headers=%v total=%v bytes=%d\n",
			r.URL, r.StatusCode, r.Headers, r.Total, r.BodyBytes)
	}

	if failed {
		os.Exit(1)
	}
}
