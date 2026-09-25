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
// proxy hold the answers to paths under PROXY_HOLD_PREFIXES (default: the erase route) N
// seconds after the upstream answered: core can be killed while a service has already
// committed, and Account Center's call to core's intake can time out after core accepted.
func runProxy() {
	var hold atomic.Int64
	var only atomic.Value // a prefix set with the hold, or "" for every configured one
	only.Store("")
	prefixes := strings.Split(os.Getenv("PROXY_HOLD_PREFIXES"), ",")
	if os.Getenv("PROXY_HOLD_PREFIXES") == "" {
		prefixes = []string{"/internal/v1/account-erasures/"}
	}
	held := func(path string) bool {
		if selected := only.Load().(string); selected != "" {
			return strings.HasPrefix(path, selected)
		}
		for _, prefix := range prefixes {
			if prefix != "" && strings.HasPrefix(path, strings.TrimSpace(prefix)) {
				return true
			}
		}
		return false
	}
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
			if seconds := hold.Load(); seconds > 0 && held(resp.Request.URL.Path) {
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
		only.Store(r.URL.Query().Get("prefix"))
		hold.Store(seconds)
		w.WriteHeader(http.StatusNoContent)
	})
	log.Fatal(http.ListenAndServe(":9091", accessLog(control)))
}
