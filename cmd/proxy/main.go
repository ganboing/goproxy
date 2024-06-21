package main

import (
	"context"
	"fmt"
	"github.com/ganboing/goproxy"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

func main() {
	addr := os.Args[1]
	idx := strings.LastIndexByte(addr, '/')
	prefix := ""
	if idx != -1 {
		prefix = addr[idx:]
		addr = addr[:idx]
	}
	proxy := &goproxy.ProxyServer{Prefix: prefix}
	server := &http.Server{
		Addr:    addr,
		Handler: proxy,
	}
	ln, err := net.Listen("tcp", server.Addr)
	if err != nil {
		log.Panicf("Failed to listen: %s", err.Error())
	}
	fmt.Fprintf(os.Stderr, "Listening on %s, Prefix=%s\n", ln.Addr().String(), prefix)
	sigchan := make(chan os.Signal)
	signal.Notify(sigchan, syscall.SIGINT, syscall.SIGTERM)
	notify := make(chan struct{})
	go func() {
		<-sigchan
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		server.Shutdown(ctx)
		notify <- struct{}{}
	}()
	server.Serve(ln)
	<-notify
}
