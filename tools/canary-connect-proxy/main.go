// Command canary-connect-proxy is a deliberately tiny CONNECT proxy for
// private benchmark VMs. Both clients and destinations are exact allowlists;
// it is not a general fleet egress service.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"time"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:3128", "listen address")
	clientsRaw := flag.String("clients", "", "comma-separated client IP allowlist")
	destinationsRaw := flag.String("destinations", "storage.googleapis.com:443", "comma-separated CONNECT destination allowlist")
	flag.Parse()
	clients := allowlist(*clientsRaw)
	destinations := allowlist(*destinationsRaw)
	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("canary CONNECT proxy listening on %s", *listen)
	for {
		conn, err := ln.Accept()
		if err != nil {
			log.Printf("accept: %v", err)
			continue
		}
		go handle(conn, clients, destinations)
	}
}

func allowlist(raw string) map[string]bool {
	out := make(map[string]bool)
	for _, value := range strings.Split(raw, ",") {
		if value = strings.TrimSpace(strings.ToLower(value)); value != "" {
			out[value] = true
		}
	}
	return out
}

func handle(client net.Conn, clients, destinations map[string]bool) {
	defer client.Close()
	clientIP, _, err := net.SplitHostPort(client.RemoteAddr().String())
	if err != nil || !clients[strings.ToLower(clientIP)] {
		return
	}
	_ = client.SetDeadline(time.Now().Add(30 * time.Second))
	// The client itself is exact-IP allowlisted. Do not wrap the connection in a
	// LimitReader: the buffered reader is reused for the tunneled request body,
	// so such a limit would silently truncate every upload at that boundary.
	reader := bufio.NewReaderSize(client, 64<<10)
	request, err := http.ReadRequest(reader)
	if err != nil || request.Method != http.MethodConnect {
		_, _ = io.WriteString(client, "HTTP/1.1 405 Method Not Allowed\r\nConnection: close\r\n\r\n")
		return
	}
	destination := strings.ToLower(request.Host)
	if !destinations[destination] {
		_, _ = io.WriteString(client, "HTTP/1.1 403 Forbidden\r\nConnection: close\r\n\r\n")
		return
	}
	upstream, err := net.DialTimeout("tcp", destination, 10*time.Second)
	if err != nil {
		_, _ = io.WriteString(client, "HTTP/1.1 502 Bad Gateway\r\nConnection: close\r\n\r\n")
		return
	}
	defer upstream.Close()
	if _, err := fmt.Fprint(client, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		return
	}
	_ = client.SetDeadline(time.Time{})
	copyDone := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(upstream, reader); copyDone <- struct{}{} }()
	go func() { _, _ = io.Copy(client, upstream); copyDone <- struct{}{} }()
	<-copyDone
}
