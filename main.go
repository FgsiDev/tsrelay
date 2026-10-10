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

// Port listener ditutup kalau berturut-turut sekian kali scan tidak ditemukan terbuka di target.
const closeAfterMisses = 3

type Config struct {
	Mode         string
	Hostname     string
	AuthKey      string
	Listen       string
	Target       string
	StateDir     string
	Ephemeral    bool
	Scan         string
	ScanInterval time.Duration
	ScanTimeout  time.Duration
	ScanWorkers  int
}

type dialFunc func(ctx context.Context, network, addr string) (net.Conn, error)
type listenFunc func(addr string) (net.Listener, error)

func main() {
	cfg := Config{}

	flag.StringVar(&cfg.Mode, "mode", "tunnel", "tunnel or target")
	flag.StringVar(&cfg.Hostname, "hostname", "tsrelay", "Tailscale hostname")
	flag.StringVar(&cfg.AuthKey, "auth-key", os.Getenv("TS_AUTHKEY"), "Tailscale auth key")
	flag.StringVar(&cfg.Listen, "listen", ":3982",
		"port listen: :2222 | 2222,8080 | 8000-8100 | 127.0.0.1:2222,8080. "+
			"IP saja (mis. 127.0.0.1) atau all = MODE OTOMATIS: port mengikuti port yang terbuka di target")
	flag.StringVar(&cfg.Target, "target", "",
		"IP/hostname tujuan. Tanpa port = port tujuan sama dengan port listen. tunnel: default 127.0.0.1")
	flag.StringVar(&cfg.StateDir, "state-dir", "", "tsnet state dir, identitas node disimpan di sini (default ~/.tsrelay)")
	flag.BoolVar(&cfg.Ephemeral, "ephemeral", false, "node otomatis dihapus dari dashboard saat offline")
	flag.StringVar(&cfg.Scan, "scan", "1-65535", "mode otomatis: port target yang dipantau (mis. 1-10000,25565)")
	flag.DurationVar(&cfg.ScanInterval, "scan-interval", 10*time.Second, "mode otomatis: jeda antar scan")
	flag.DurationVar(&cfg.ScanTimeout, "scan-timeout", 700*time.Millisecond, "mode otomatis: timeout cek tiap port")
	flag.IntVar(&cfg.ScanWorkers, "scan-workers", 512, "mode otomatis: jumlah cek port paralel")
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

	// ---- parse --listen: host + daftar port (atau mode otomatis) ----
	listenHost, spec := splitListen(cfg.Listen)
	auto := spec == "all"

	var (
		err       error
		ports     []int // mode statis
		scanPorts []int // mode otomatis
	)
	if auto {
		scanPorts, err = parsePorts(cfg.Scan)
		if err != nil {
			log.Fatalf("--scan tidak valid: %v", err)
		}
	} else {
		ports, err = parsePorts(spec)
		if err != nil {
			log.Fatalf("--listen tidak valid: %v", err)
		}
	}

	// Naikkan batas file descriptor; separuhnya boleh dipakai listener, sisanya untuk koneksi.
	fdLimit := raiseNoFileLimit()
	budget := int(fdLimit / 2)
	if budget < 16 {
		budget = 16
	}
	workers := cfg.ScanWorkers
	if m := int(fdLimit / 4); workers > m {
		workers = m
	}
	if workers < 16 {
		workers = 16
	}
	if !auto && len(ports) > budget {
		log.Fatalf("terlalu banyak port (%d). Batas perangkat ini sekitar %d port listener "+
			"(file descriptor = %d). Kurangi jumlah port", len(ports), budget, fdLimit)
	}

	// Alamat bind lokal (mode target) harus milik perangkat ini.
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
	if targetPort != "" && (auto || len(ports) > 1) {
		log.Fatal("--target dengan port hanya untuk satu port listen. " +
			"Untuk multi-port / mode otomatis tulis IP/hostname saja (port tujuan = port listen)")
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
	if auto {
		log.Printf("Listen   : %s (OTOMATIS: port mengikuti port terbuka di target)", hostLabel(listenHost))
		log.Printf("Target   : %s", targetHost)
		log.Printf("Scan     : %s tiap %s (timeout %s, %d paralel, maks %d port aktif)",
			cfg.Scan, cfg.ScanInterval, cfg.ScanTimeout, workers, budget)
	} else {
		log.Printf("Listen   : %s", describeListen(listenHost, ports))
		log.Printf("Target   : %s", describeTarget(targetHost, targetPort))
	}
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

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sig
		log.Printf("shutdown...")
		cancel()
	}()

	// ---- mode otomatis ----
	if auto {
		ar := &autoRelay{
			listen:   listen,
			dial:     dial,
			host:     listenHost,
			tHost:    targetHost,
			ports:    scanPorts,
			budget:   budget,
			interval: cfg.ScanInterval,
			timeout:  cfg.ScanTimeout,
			workers:  workers,
			active:   map[int]net.Listener{},
			missing:  map[int]int{},
			skipped:  map[int]bool{},
		}
		ar.run(ctx)
		return
	}

	// ---- mode statis: buka semua listener ----
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

	go func() {
		<-ctx.Done()
		for _, ln := range lns {
			ln.Close()
		}
	}()

	wg.Wait()
}

// ---------------------------------------------------------------------------
// Mode otomatis: pantau port terbuka di target, buka/tutup listener mengikutinya.
// ---------------------------------------------------------------------------

type autoRelay struct {
	listen   listenFunc
	dial     dialFunc
	host     string // alamat bind listener
	tHost    string // host target
	ports    []int  // port yang dipantau
	budget   int    // maks listener aktif
	interval time.Duration
	timeout  time.Duration
	workers  int

	mu      sync.Mutex
	active  map[int]net.Listener
	missing map[int]int
	skipped map[int]bool
	wg      sync.WaitGroup
}

func (r *autoRelay) run(ctx context.Context) {
	first := true
	for {
		start := time.Now()
		seen := r.scan(ctx)
		if ctx.Err() != nil {
			break
		}
		r.prune(seen)

		if first {
			first = false
			log.Printf("scan pertama selesai (%s): %d port terbuka di target, %d port aktif",
				time.Since(start).Round(time.Second), len(seen), r.activeCount())
		}

		select {
		case <-ctx.Done():
		case <-time.After(r.interval):
		}
		if ctx.Err() != nil {
			break
		}
	}
	r.closeAll()
	r.wg.Wait()
}

// scan memeriksa semua port; port yang terbuka langsung dibuatkan listener (progresif).
func (r *autoRelay) scan(ctx context.Context) map[int]bool {
	seen := map[int]bool{}
	var smu sync.Mutex

	jobs := make(chan int, 1024)
	var wg sync.WaitGroup
	for i := 0; i < r.workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for p := range jobs {
				if ctx.Err() != nil {
					continue
				}
				if r.probe(ctx, p) {
					smu.Lock()
					seen[p] = true
					smu.Unlock()
					r.ensure(p)
				}
			}
		}()
	}
	for _, p := range r.ports {
		if ctx.Err() != nil {
			break
		}
		jobs <- p
	}
	close(jobs)
	wg.Wait()
	return seen
}

func (r *autoRelay) probe(ctx context.Context, p int) bool {
	cctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	c, err := r.dial(cctx, "tcp", net.JoinHostPort(r.tHost, strconv.Itoa(p)))
	if err != nil {
		return false
	}
	c.Close()
	return true
}

// ensure memastikan listener untuk port p aktif.
func (r *autoRelay) ensure(p int) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, ok := r.active[p]; ok {
		r.missing[p] = 0
		return
	}
	if r.skipped[p] {
		return
	}
	if len(r.active) >= r.budget {
		r.skipped[p] = true
		log.Printf("port %d dilewati: batas %d listener aktif tercapai", p, r.budget)
		return
	}

	ln, err := r.listen(net.JoinHostPort(r.host, strconv.Itoa(p)))
	if err != nil {
		r.skipped[p] = true
		log.Printf("port %d dilewati: %v", p, err)
		return
	}

	r.active[p] = ln
	r.missing[p] = 0
	target := net.JoinHostPort(r.tHost, strconv.Itoa(p))
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		serve(ln, r.dial, target)
	}()
	log.Printf("+ port %d dibuka -> %s", p, target)
}

// prune menutup listener yang portnya tidak terlihat lagi di target.
func (r *autoRelay) prune(seen map[int]bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	for p, ln := range r.active {
		if seen[p] {
			r.missing[p] = 0
			continue
		}
		r.missing[p]++
		if r.missing[p] >= closeAfterMisses {
			ln.Close()
			delete(r.active, p)
			delete(r.missing, p)
			log.Printf("- port %d ditutup (target tidak membuka port ini lagi)", p)
		}
	}
	for p := range r.skipped {
		if !seen[p] {
			delete(r.skipped, p) // dicoba lagi kalau port itu muncul kembali
		}
	}
}

func (r *autoRelay) activeCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.active)
}

func (r *autoRelay) closeAll() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for p, ln := range r.active {
		ln.Close()
		delete(r.active, p)
	}
}

// ---------------------------------------------------------------------------
// Parsing & util
// ---------------------------------------------------------------------------

// splitListen memecah --listen menjadi host bind + spesifikasi port.
//
//	":2222"                -> "", "2222"
//	"2222,8080"            -> "", "2222,8080"
//	"127.0.0.1:8000-8100"  -> "127.0.0.1", "8000-8100"
//	"127.0.0.1" / "all"    -> host / "", "all"  (mode otomatis)
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
	return s, "all" // IP/host saja -> mode otomatis
}

func isPortSpec(s string) bool {
	for _, r := range s {
		if !(r >= '0' && r <= '9') && r != ',' && r != '-' {
			return false
		}
	}
	return s != ""
}

// parsePorts: "2222" | "2222,8080" | "8000-8100" | gabungan.
func parsePorts(spec string) ([]int, error) {
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

func hostLabel(host string) string {
	if host == "" {
		return "*"
	}
	return host
}

func describeListen(host string, ports []int) string {
	h := hostLabel(host)
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

// raiseNoFileLimit menaikkan batas file descriptor (soft) ke batas maksimum
// yang diizinkan OS, lalu mengembalikan nilai soft limit yang berlaku.
func raiseNoFileLimit() uint64 {
	var lim syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &lim); err != nil {
		return 1024
	}
	want := lim.Max
	if want > 1<<20 {
		want = 1 << 20
	}
	if want > lim.Cur {
		next := lim
		next.Cur = want
		if err := syscall.Setrlimit(syscall.RLIMIT_NOFILE, &next); err == nil {
			lim = next
		}
	}
	return lim.Cur
}

var (
	fdWarnMu sync.Mutex
	fdWarnAt time.Time
)

// warnTooManyFiles mencatat peringatan fd habis paling sering sekali per 5 detik.
func warnTooManyFiles() {
	fdWarnMu.Lock()
	defer fdWarnMu.Unlock()
	if time.Since(fdWarnAt) < 5*time.Second {
		return
	}
	fdWarnAt = time.Now()
	log.Printf("file descriptor habis (too many open files): kurangi jumlah port listen atau jumlah koneksi bersamaan")
}

func serve(ln net.Listener, dial dialFunc, target string) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			if errors.Is(err, syscall.EMFILE) || errors.Is(err, syscall.ENFILE) {
				warnTooManyFiles()
				time.Sleep(time.Second)
				continue
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
