package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
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
	Mode      string
	Hostname  string
	AuthKey   string
	Listen    string
	Target    string
	StateDir  string
	Ephemeral bool
}

type dialFunc func(ctx context.Context, network, addr string) (net.Conn, error)

func main() {
	cfg := Config{}

	flag.StringVar(&cfg.Mode, "mode", "tunnel", "tunnel or target")
	flag.StringVar(&cfg.Hostname, "hostname", "tsrelay", "Tailscale hostname")
	flag.StringVar(&cfg.AuthKey, "auth-key", os.Getenv("TS_AUTHKEY"), "Tailscale auth key")
	flag.StringVar(&cfg.Listen, "listen", ":3982", "listen address / port")
	flag.StringVar(&cfg.Target, "target", "", "IP/hostname tujuan, port opsional (default = port listen). tunnel: default 127.0.0.1")
	flag.StringVar(&cfg.StateDir, "state-dir", "", "tsnet state dir, identitas node disimpan di sini (default ~/.tsrelay)")
	flag.BoolVar(&cfg.Ephemeral, "ephemeral", false, "node otomatis dihapus dari dashboard saat offline")
	flag.Parse()

	if cfg.Mode != "tunnel" && cfg.Mode != "target" {
		log.Fatal("--mode harus tunnel atau target")
	}
	if cfg.Target == "" {
		if cfg.Mode == "target" {
			log.Fatal("--target wajib diisi di mode target")
		}
		cfg.Target = "127.0.0.1" // tunnel (sisi VPS): default ke service lokal VPS
	}
	// --listen boleh ":3982" atau "3982"
	if !strings.Contains(cfg.Listen, ":") {
		cfg.Listen = ":" + cfg.Listen
	}
	_, listenPort, err := net.SplitHostPort(cfg.Listen)
	if err != nil {
		log.Fatalf("--listen tidak valid: %v", err)
	}

	// Target tanpa port -> pakai port yang sama dengan --listen
	cfg.Target = normalizeTarget(cfg.Target, listenPort)

	// State dir unik per hostname + dikunci agar 1 state hanya dipakai 1 proses
	if cfg.StateDir == "" {
		home, herr := os.UserHomeDir()
		if herr != nil {
			home = "."
		}
		cfg.StateDir = filepath.Join(home, ".tsrelay")
	}
	if err := os.MkdirAll(cfg.StateDir, 0o700); err != nil {
		log.Fatalf("gagal buat state dir: %v", err)
	}
	lock, err := lockDir(cfg.StateDir)
	if err != nil {
		log.Fatal(err)
	}
	defer lock.Close()

	// Auth key hanya wajib saat belum ada state (login pertama)
	if cfg.AuthKey == "" {
		if _, serr := os.Stat(filepath.Join(cfg.StateDir, "tailscaled.state")); serr != nil {
			log.Fatal("TS_AUTHKEY belum diisi")
		}
	}

	srv := &tsnet.Server{
		Hostname:  cfg.Hostname,
		AuthKey:   cfg.AuthKey,
		Dir:       cfg.StateDir,
		Ephemeral: cfg.Ephemeral,
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
	log.Printf("State    : %s", cfg.StateDir)
	log.Printf("=================================")

	var (
		ln   net.Listener
		dial dialFunc
	)

	if cfg.Mode == "tunnel" {
		// Tailnet:Listen -> Target (dial biasa dari mesin ini)
		ln, err = srv.Listen("tcp", cfg.Listen)
		if err != nil {
			log.Fatalf("gagal listen Tailnet: %v", err)
		}
		d := &net.Dialer{}
		dial = d.DialContext
		log.Printf("Tunnel aktif: Tailscale%s -> %s", cfg.Listen, cfg.Target)
	} else {
		// Lokal:Listen -> Target (lewat Tailnet)
		ln, err = net.Listen("tcp", cfg.Listen)
		if err != nil {
			log.Fatalf("gagal listen lokal: %v", err)
		}
		dial = srv.Dial
		log.Printf("Target aktif: %s -> %s (via Tailscale)", cfg.Listen, cfg.Target)
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

// normalizeTarget: "100.1.2.3" / "host" -> tambah defPort; "host:8080" tetap.
func normalizeTarget(target, defPort string) string {
	if ip := net.ParseIP(strings.Trim(target, "[]")); ip != nil {
		return net.JoinHostPort(ip.String(), defPort)
	}
	if _, _, err := net.SplitHostPort(target); err == nil {
		return target
	}
	return net.JoinHostPort(target, defPort)
}

// lockDir mencegah dua proses memakai state (node key) yang sama.
func lockDir(dir string) (*os.File, error) {
	f, err := os.OpenFile(filepath.Join(dir, "tsrelay.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("gagal buka lock file: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("state dir %s sedang dipakai instance lain (1 state = 1 proses)", dir)
	}
	return f, nil
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
