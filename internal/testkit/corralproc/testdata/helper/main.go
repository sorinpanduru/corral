// Command helper is a stand-in for the corral binary used by the corralproc
// self-test. It prints JSON log records, serves /readyz and /metrics, honours
// the CORRALTEST_HOLD_* protocol and counts signals. Synthetic only.
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func log(kv map[string]any) {
	b, _ := json.Marshal(kv)
	fmt.Println(string(b))
}

func main() {
	log(map[string]any{"msg": "started", "component": "lifecycle", "args": strings.Join(os.Args[1:], " "), "env_marker": os.Getenv("HELPER_MARKER")})
	fmt.Println("not json: plain stdout line")
	fmt.Fprintln(os.Stderr, "stderr line")

	if c := os.Getenv("HELPER_EXIT_CODE"); c != "" {
		n, _ := strconv.Atoi(c)
		os.Exit(n)
	}

	if addr := os.Getenv("HELPER_ADMIN"); addr != "" {
		mux := http.NewServeMux()
		mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
			if _, err := os.Stat(filepath.Join(os.Getenv("CORRALTEST_HOLD_DIR"), "notready")); err == nil {
				w.WriteHeader(503)
				fmt.Fprint(w, "starting\n")
				return
			}
			fmt.Fprint(w, "ok\n")
		})
		mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, "# HELP helper_up Synthetic.\n# TYPE helper_up gauge\nhelper_up 1\n")
		})
		go http.ListenAndServe(addr, mux)
	}

	need := 1
	if n := os.Getenv("HELPER_NEED_SIGNALS"); n != "" {
		need, _ = strconv.Atoi(n)
	}
	sigs := make(chan os.Signal, 64)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT, syscall.SIGUSR1)

	dir := os.Getenv("CORRALTEST_HOLD_DIR")
	if hold := os.Getenv("CORRALTEST_HOLD_AT"); hold != "" && dir != "" {
		for _, p := range strings.Split(hold, ",") {
			held, rel := "held-"+p, "release-"+p
			if p == "logger" || p == "checkconfig" {
				held, rel = "held", "release"
			}
			os.WriteFile(filepath.Join(dir, held), nil, 0o600)
			log(map[string]any{"msg": "holding", "component": "lifecycle", "point": p})
			for {
				if _, err := os.Stat(filepath.Join(dir, rel)); err == nil {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			log(map[string]any{"msg": "released", "component": "lifecycle", "point": p})
		}
	}
	log(map[string]any{"msg": "ready", "component": "lifecycle"})

	count := 0
	for s := range sigs {
		if s == syscall.SIGUSR1 {
			continue
		}
		count++
		log(map[string]any{"msg": "signal", "component": "lifecycle", "count": count})
		if count >= need {
			os.Exit(7)
		}
	}
}
