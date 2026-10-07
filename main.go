package main

import (
	"context"
	"errors"
	"flag"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"tailscale.com/tsnet"
)

type Config struct {
	Mode     string
	Hostname string
	AuthKey  string
	Listen   string
	Target   string
	StateDir string
}

type dialFunc func(ctx context.Context, network, addr string) (net.Conn, error)

func main() {
	cfg := Config{}

	flag.StringVar(&cfg.Mode, "mode", "tunnel", "tunnel or target")
	flag.StringVar(&cfg.Hostname, "hostname", "tsrelay", "Tailscale hostname")
	flag.StringVar(&cfg.AuthKey, "auth-key", os.Getenv("TS_AUTHKEY"), "Tailscale auth key")
	flag.StringVar(&cfg.Listen, "listen", ":3982", "listen address")
	flag.StringVar(&cfg.Target, "target", "", "target address")
	flag.StringVar(&cfg.StateDir, "state-dir", "", "tsnet state dir (default ~/.tsrelay/<hostname>)")
	flag.Parse()

	if cfg.AuthKey == "" {
		log.Fatal("TS_AUTHKEY belum diisi")
	}
	if cfg.Target == "" {
		log.Fatal("--target wajib diisi")
	}
	if cfg.Mode != "tunnel" && cfg.Mode != "target" {
		log.Fatal("--mode harus tunnel atau target")
	}

	if cfg.StateDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			home = "."
		}
		cfg.StateDir = filepath.Join(home, ".tsrelay", cfg.Hostname)
	}
	if err := os.MkdirAll(cfg.StateDir, 0o700); err != nil {
		log.Fatalf("gagal buat state dir: %v", err)
	}

	srv := &tsnet.Server{
		Hostname: cfg.Hostname,
		AuthKey:  cfg.AuthKey,
		Dir:      cfg.StateDir,
	}
	defer srv.Close()

	upCtx, upCancel := context.WithTimeout(context.Background(), 90*time.Second)
	status, err := srv.Up(upCtx)
	upCancel()
	if err != nil {
		log.Fatalf("Tailscale gagal connect: %v", err)
	}

	var ips []string
	for _, ip := range status.TailscaleIPs {
		ips = append(ips, ip.String())
	}

	log.Printf("=================================")
	log.Printf("TSRelay")
	log.Printf("Mode     : %s", cfg.Mode)
	log.Printf("Hostname : %s", cfg.Hostname)
	log.Printf("TS IP    : %s", strings.Join(ips, ", "))
	log.Printf("Listen   : %s", cfg.Listen)
	log.Printf("Target   : %s", cfg.Target)
	log.Printf("=================================")

	var (
		ln   net.Listener
		dial dialFunc
	)

	if cfg.Mode == "tunnel" {
		// Tailnet:Listen -> Target (lokal/publik, dial biasa)
		ln, err = srv.Listen("tcp", cfg.Listen)
		if err != nil {
			log.Fatalf("gagal listen Tailnet: %v", err)
		}
		d := &net.Dialer{}
		dial = d.DialContext
		log.Printf("Tunnel aktif: Tailscale%s -> %s", cfg.Listen, cfg.Target)
	} else {
		// Publik:Listen -> Target (lewat Tailnet)
		ln, err = net.Listen("tcp", cfg.Listen)
		if err != nil {
			log.Fatalf("gagal listen public: %v", err)
		}
		dial = srv.Dial
		log.Printf("Target relay aktif: %s -> Tailscale:%s", cfg.Listen, cfg.Target)
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sig
		log.Printf("shutdown...")
		ln.Close()
	}()

	serve(ln, dial, cfg.Target)
}

func serve(ln net.Listener, dial dialFunc, target string) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			log.Printf("accept error: %v", err)
			time.Sleep(100 * time.Millisecond)
			continue
		}
		go handleConnection(conn, dial, target)
	}
}

func handleConnection(client net.Conn, dial dialFunc, target string) {
	defer client.Close()

	log.Printf("Koneksi dari %s", client.RemoteAddr())

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	targetConn, err := dial(ctx, "tcp", target)
	if err != nil {
		log.Printf("gagal connect target %s: %v", target, err)
		return
	}
	defer targetConn.Close()

	proxy(client, targetConn)
}

type closeWriter interface{ CloseWrite() error }

func proxy(a, b net.Conn) {
	var wg sync.WaitGroup
	wg.Add(2)

	pipe := func(dst, src net.Conn) {
		defer wg.Done()
		_, _ = io.Copy(dst, src)
		if cw, ok := dst.(closeWriter); ok {
			_ = cw.CloseWrite()
		} else {
			_ = dst.Close()
		}
	}

	go pipe(b, a)
	go pipe(a, b)

	wg.Wait()
}
