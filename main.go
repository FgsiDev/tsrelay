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
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"tailscale.com/tsnet"
)

// Batas bawah port untuk mode "semua port" (port <1024 butuh root di OS).
const allPortsFrom = 1024

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
type listenFunc func(addr string) (net.Listener, error)

func main() {
	cfg := Config{}

	flag.StringVar(&cfg.Mode, "mode", "tunnel", "tunnel or target")
	flag.StringVar(&cfg.Hostname, "hostname", "tsrelay", "Tailscale hostname")
	flag.StringVar(&cfg.AuthKey, "auth-key", os.Getenv("TS_AUTHKEY"), "Tailscale auth key")
	flag.StringVar(&cfg.Listen, "listen", ":3982",
		"port listen: :2222 | 2222,8080 | 8000-8100 | 127.0.0.1:2222,8080 | IP saja / all = semua port (>=1024)")
	flag.StringVar(&cfg.Target, "target", "",
		"IP/hostname tujuan. Tanpa port = port tujuan sama dengan port listen (multi-port). tunnel: default 127.0.0.1")
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

	// ---- parse --listen: host + daftar port ----
	listenHost, spec := splitListen(cfg.Listen)
	ports, err := parsePorts(spec)
	if err != nil {
		log.Fatalf("--listen tidak valid: %v", err)
	}
	hostOnly := spec == "all"

	// Host-only di mode target = alamat bind lokal, harus milik perangkat ini.
	if cfg.Mode == "target" && listenHost != "" {
		probe, perr := net.Listen("tcp", net.JoinHostPort(listenHost, "0"))
		if perr != nil {
			log.Fatalf("--listen %q bukan alamat milik perangkat ini (%v). "+
				"--listen = alamat LOKAL tempat server dibuka (mis. 127.0.0.1), "+
				"IP tujuan ditulis di --target", listenHost, perr)
		}
		probe.Close()
	}

	// ---- parse --target ----
	targetHost, targetPort := splitTarget(cfg.Target)
	if targetHost == "" {
		log.Fatal("--target tidak valid")
	}
	if targetPort != "" && len(ports) > 1 {
		log.Fatal("--target dengan port hanya untuk satu port listen. " +
			"Untuk multi-port tulis IP/hostname saja (port tujuan = port listen)")
	}
	targetFor := func(p int) string {
		if targetPort != "" {
			return net.JoinHostPort(targetHost, targetPort)
		}
		return net.JoinHostPort(targetHost, strconv.Itoa(p))
	}

	// ---- state dir + lock ----
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
	log.Printf("Listen   : %s", describeListen(listenHost, ports, hostOnly))
	log.Printf("Target   : %s", describeTarget(targetHost, targetPort))
	log.Printf("State    : %s", cfg.StateDir)
	log.Printf("=================================")

	var (
		listen listenFunc
		dial   dialFunc
	)
	if cfg.Mode == "tunnel" {
		// Tailnet:port -> target (dial biasa dari mesin ini)
		listen = func(addr string) (net.Listener, error) { return srv.Listen("tcp", addr) }
		d := &net.Dialer{}
		dial = d.DialContext
	} else {
		// Lokal:port -> target (lewat Tailnet)
		listen = func(addr string) (net.Listener, error) { return net.Listen("tcp", addr) }
		dial = srv.Dial
	}

	// ---- buka semua listener ----
	var (
		wg      sync.WaitGroup
		lns     []net.Listener
		failed  []int
		lastErr error
	)
	for _, p := range ports {
		ln, lerr := listen(net.JoinHostPort(listenHost, strconv.Itoa(p)))
		if lerr != nil {
			failed = append(failed, p)
			lastErr = lerr
			continue
		}
		lns = append(lns, ln)
		wg.Add(1)
		go func(ln net.Listener, target string) {
			defer wg.Done()
			serve(ln, dial, target)
		}(ln, targetFor(p))
	}

	if len(lns) == 0 {
		log.Fatalf("tidak ada port yang berhasil dibuka: %v", lastErr)
	}

	log.Printf("Aktif: %d port dibuka", len(lns))
	if len(failed) > 0 {
		if len(failed) <= 20 {
			log.Printf("Dilewati (%d port gagal/sudah terpakai): %v", len(failed), failed)
		} else {
			log.Printf("Dilewati: %d port gagal/sudah terpakai (contoh error: %v)", len(failed), lastErr)
		}
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sig
		log.Printf("shutdown...")
		for _, ln := range lns {
			ln.Close()
		}
	}()

	wg.Wait()
}

// splitListen memecah --listen menjadi host bind + spesifikasi port.
//
//	":2222"                -> "", "2222"
//	"2222,8080"            -> "", "2222,8080"
//	"127.0.0.1:8000-8100"  -> "127.0.0.1", "8000-8100"
//	"127.0.0.1" / "all"    -> host / "", "all"
func splitListen(s string) (host, spec string) {
	s = strings.TrimSpace(s)
	if s == "" || s == "all" || s == "*" {
		return "", "all"
	}
	if i := strings.LastIndex(s, ":"); i >= 0 {
		host, spec = s[:i], s[i+1:]
		if spec == "" || spec == "*" {
			spec = "all"
		}
		return strings.Trim(host, "[]"), spec
	}
	if isPortSpec(s) {
		return "", s
	}
	return s, "all" // IP/host saja -> semua port
}

func isPortSpec(s string) bool {
	for _, r := range s {
		if !(r >= '0' && r <= '9') && r != ',' && r != '-' {
			return false
		}
	}
	return s != ""
}

// parsePorts: "all" | "2222" | "2222,8080" | "8000-8100" | gabungan.
func parsePorts(spec string) ([]int, error) {
	if spec == "all" {
		ports := make([]int, 0, 65535-allPortsFrom+1)
		for p := allPortsFrom; p <= 65535; p++ {
			ports = append(ports, p)
		}
		return ports, nil
	}

	seen := map[int]bool{}
	var ports []int
	for _, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		a, b := part, part
		if i := strings.Index(part, "-"); i >= 0 {
			a, b = part[:i], part[i+1:]
		}
		start, e1 := strconv.Atoi(a)
		end, e2 := strconv.Atoi(b)
		if e1 != nil || e2 != nil || start < 1 || end > 65535 || start > end {
			return nil, fmt.Errorf("port %q salah (harus 1-65535, contoh 2222 / 8000-8100)", part)
		}
		for p := start; p <= end; p++ {
			if !seen[p] {
				seen[p] = true
				ports = append(ports, p)
			}
		}
	}
	if len(ports) == 0 {
		return nil, errors.New("tidak ada port")
	}
	return ports, nil
}

// splitTarget: "100.1.2.3" -> (host, ""), "100.1.2.3:22" -> (host, "22").
func splitTarget(t string) (host, port string) {
	t = strings.TrimSpace(t)
	if h, p, err := net.SplitHostPort(t); err == nil {
		return h, p
	}
	return strings.Trim(t, "[]"), ""
}

func describeListen(host string, ports []int, hostOnly bool) string {
	h := host
	if h == "" {
		h = "*"
	}
	if hostOnly {
		return fmt.Sprintf("%s:%d-65535 (semua port, %d port)", h, allPortsFrom, len(ports))
	}
	if len(ports) <= 10 {
		strs := make([]string, len(ports))
		for i, p := range ports {
			strs[i] = strconv.Itoa(p)
		}
		return fmt.Sprintf("%s:%s", h, strings.Join(strs, ","))
	}
	return fmt.Sprintf("%s: %d port (%d ... %d)", h, len(ports), ports[0], ports[len(ports)-1])
}

func describeTarget(host, port string) string {
	if port == "" {
		return host + " (port tujuan = port listen)"
	}
	return net.JoinHostPort(host, port)
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

	log.Printf("Koneksi dari %s -> %s", client.RemoteAddr(), target)

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
