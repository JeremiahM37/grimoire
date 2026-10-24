package main

import (
	"log"
	"net"
	"net/http"
	"net/http/pprof"
	"os"
)

// startPprof serves Go's profiler on GRIMOIRE_PPROF (a loopback host:port) when
// it is set. It is off by default and refuses any address that is not
// loopback: a profile shows request paths and timings, which is not something
// to publish on the tailnet.
func startPprof() {
	addr := os.Getenv("GRIMOIRE_PPROF")
	if addr == "" {
		return
	}
	host, _, err := net.SplitHostPort(addr)
	if ip := net.ParseIP(host); err != nil || ip == nil || !ip.IsLoopback() {
		log.Printf("GRIMOIRE_PPROF=%q ignored: must be a loopback host:port", addr)
		return
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	go func() {
		if err := http.ListenAndServe(addr, mux); err != nil {
			log.Printf("pprof: %v", err)
		}
	}()
}
