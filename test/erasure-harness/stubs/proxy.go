package main

import (
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// runProxy forwards PROXY_ROUTES ("listen=upstream,listen=upstream", e.g.
// ":9001=http://skymail:3000") unchanged. POST /harness/hold?seconds=N on :9091 makes every
// proxy hold the next erase responses N seconds after the upstream answered, so core can be
// killed while the service has already committed and core has not seen the answer.
func runProxy() {
	var hold atomic.Int64
	routes := strings.Split(os.Getenv("PROXY_ROUTES"), ",")
	for _, route := range routes {
		listen, upstream, ok := strings.Cut(strings.TrimSpace(route), "=")
		if !ok {
			log.Fatalf("PROXY_ROUTES entry %q is not listen=upstream", route)
		}
		target, err := url.Parse(upstream)
		if err != nil {
			log.Fatalf("upstream %q: %v", upstream, err)
		}
		proxy := httputil.NewSingleHostReverseProxy(target)
		proxy.ModifyResponse = func(resp *http.Response) error {
			if seconds := hold.Load(); seconds > 0 && strings.HasPrefix(resp.Request.URL.Path, "/internal/v1/account-erasures/") {
				log.Printf("harness hold %s seconds=%d upstream_status=%d", resp.Request.URL.Path, seconds, resp.StatusCode)
				time.Sleep(time.Duration(seconds) * time.Second)
			}
			return nil
		}
		// httputil adds X-Forwarded-For, which the services answer with 404 on this route.
		director := proxy.Director
		proxy.Director = func(r *http.Request) {
			director(r)
			r.Header["X-Forwarded-For"] = nil
		}
		go func(listen string, handler http.Handler) {
			log.Fatal(http.ListenAndServe(listen, accessLog(handler)))
		}(listen, proxy)
		log.Printf("proxy %s -> %s", listen, upstream)
	}
	control := http.NewServeMux()
	control.HandleFunc("POST /harness/hold", func(w http.ResponseWriter, r *http.Request) {
		seconds, _ := strconv.ParseInt(r.URL.Query().Get("seconds"), 10, 64)
		hold.Store(seconds)
		w.WriteHeader(http.StatusNoContent)
	})
	log.Fatal(http.ListenAndServe(":9091", accessLog(control)))
}
