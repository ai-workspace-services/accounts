// Command stripemock serves fixture-backed Stripe API responses for lanes whose
// NetworkPolicy denies egress to api.stripe.com. Point accounts at it with
// STRIPE_API_BASE_URL=http://<svc>:<port>/v1 and a test-mode key; live keys are
// refused by both accounts and the mock.
package main

import (
	"flag"
	"log"
	"net/http"
	"time"

	"account/internal/stripemock"
)

func main() {
	addr := flag.String("listen", ":12111", "listen address")
	fixtures := flag.String("fixtures", "", "JSON file mapping paths (optionally prefixed by METHOD) to Stripe objects")
	flag.Parse()
	m := stripemock.New(nil)
	if *fixtures != "" {
		loaded, err := stripemock.LoadFile(*fixtures)
		if err != nil {
			log.Fatalf("load fixtures: %v", err)
		}
		m = stripemock.New(loaded)
	}
	srv := &http.Server{Addr: *addr, Handler: m, ReadHeaderTimeout: 5 * time.Second}
	log.Printf("stripemock listening on %s", *addr)
	log.Fatal(srv.ListenAndServe())
}
