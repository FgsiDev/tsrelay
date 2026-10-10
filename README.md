# TSRelay

Relay TCP sederhana di atas Tailscale (`tsnet`). Dipakai untuk mengakses service di VPS lewat jaringan Tailscale, bukan lewat IP publik VPS. Satu binary static, jalan di Termux (32/64-bit) dan VPS (x86/x86_64/ARM). Mendukung satu port, daftar port, rentang port, atau semua port sekaligus.

```
[Local / Termux]                              [VPS]
localhost:3982  ──── Tailscale ────►  tailnet:3982 ──► 127.0.0.1:3982
   --mode target                         --mode tunnel
```

## Mode

| Mode | Dijalankan di | Fungsi |
|------|---------------|--------|
| `tunnel` | VPS (sumber service) | Listen di Tailnet, meneruskan ke `--target` (default `127.0.0.1`) |
| `target` | Local / Termux | Listen di lokal, meneruskan ke `--target` lewat Tailnet |

`--listen` adalah alamat **lokal** tempat server dibuka. `--target` adalah tujuan akhir. Port di `--target` bersifat opsional: kalau tidak ditulis, port tujuan sama dengan port listen.

## Install

Binary otomatis sesuai arsitektur perangkat:

```bash
curl -fsSL https://raw.githubusercontent.com/FgsiDev/tsrelay/main/install.sh | bash
```

Nama file output sendiri:

```bash
curl -fsSL https://raw.githubusercontent.com/FgsiDev/tsrelay/main/install.sh | bash -s -- nama-file
```

Binary yang tersedia di Release:

| File | Perangkat |
|------|-----------|
| `tsrelay-linux-amd64` | VPS x86_64 |
| `tsrelay-linux-386` | VPS x86 32-bit |
| `tsrelay-linux-arm64` | Termux 64-bit, VPS ARM |
| `tsrelay-linux-armv7` | Termux 32-bit |
| `tsrelay-linux-armv6` | ARM 32-bit lama |

## Pemakaian

Buat auth key di Tailscale admin console (Settings → Keys). Disarankan **reusable**.

**Di VPS:**

```bash
export TS_AUTHKEY=tskey-auth-xxxx
./tsrelay --mode tunnel --listen :3982 --target 127.0.0.1
```

Kalau service di VPS memakai port lain: `--target 127.0.0.1:8080`.

**Di Local / Termux:**

```bash
export TS_AUTHKEY=tskey-auth-xxxx
./tsrelay --mode target --listen :3982 --target 100.x.x.x
```

`100.x.x.x` adalah IP Tailscale VPS. Hostname MagicDNS juga bisa. Setelah itu `localhost:3982` langsung mengakses service di VPS.

Contoh SSH ke VPS lewat relay (SSH di port 22, dibuka lokal di 2222 karena Termux tanpa root tidak boleh bind port di bawah 1024):

```bash
./tsrelay --mode target --listen :2222 --target 100.x.x.x:22
ssh root@127.0.0.1 -p 2222
```

## Multi-port

Format `--listen`:

| Nilai | Artinya |
|-------|---------|
| `:2222` atau `2222` | Satu port |
| `2222,8080,3000` | Beberapa port |
| `8000-8100` | Rentang port |
| `2222,8000-8100` | Gabungan |
| `127.0.0.1:2222,8080` | Bind ke alamat tertentu |
| `127.0.0.1` atau `all` | Semua port 1024-65535 |

Contoh:

```bash
# tiga port, port tujuan sama dengan port listen
./tsrelay --mode target --listen 2222,8080,3000 --target 100.x.x.x

# rentang port
./tsrelay --mode target --listen 8000-8100 --target 100.x.x.x

# semua port (1024-65535) di localhost
./tsrelay --mode target --listen 127.0.0.1 --target 100.x.x.x
```

Aturan:

- `--target` tanpa port: `localhost:X` diteruskan ke `100.x.x.x:X` untuk setiap port X yang dibuka.
- `--target` dengan port (`100.x.x.x:22`) hanya boleh dipakai dengan **satu** port listen. Kalau dipakai bersama banyak port, program langsung berhenti dengan pesan error.
- Port yang gagal dibuka atau sudah terpakai dilewati dan dilaporkan di log. Program tetap jalan selama minimal satu port berhasil.
- Mode semua port tidak mencakup port di bawah 1024. Kalau di VPS dengan root butuh port 22, tulis eksplisit: `--listen 22,1024-65535`.
- Mode semua port membuka sekitar 64 ribu listener. Batas file descriptor di HP bisa membuat sebagian gagal. Kalau begitu, pakai daftar atau rentang port yang dibutuhkan saja.
- Port tujuan yang tidak ada servicenya akan menghasilkan `connection refused` di log. Itu normal, relay hanya meneruskan.

## Flag

| Flag | Default | Keterangan |
|------|---------|------------|
| `--mode` | `tunnel` | `tunnel` atau `target` |
| `--listen` | `:3982` | Port listen: satu, daftar, rentang, atau semua. Lihat bagian Multi-port |
| `--target` | - | IP atau hostname tujuan, port opsional. Wajib di mode `target`. Di `tunnel` default `127.0.0.1` |
| `--hostname` | `tsrelay` | Nama node di dashboard Tailscale |
| `--auth-key` | `$TS_AUTHKEY` | Auth key Tailscale. Hanya wajib saat login pertama |
| `--state-dir` | `~/.tsrelay` | Lokasi identitas node (node key) |
| `--ephemeral` | `false` | Node otomatis hilang dari dashboard saat offline |

## Identitas node (supaya tidak muncul machine baru)

TSRelay mendaftar sebagai satu node Tailscale. Identitasnya disimpan di `--state-dir`.

- Selama `~/.tsrelay` tidak dihapus, setiap restart tetap menjadi machine dan IP yang sama.
- Setelah login pertama, `TS_AUTHKEY` tidak diperlukan lagi.
- Satu state hanya boleh dipakai satu proses (ada lock file). Instance kedua dengan state yang sama akan ditolak.
- Jangan menyalin folder state ke perangkat lain. Itu penyebab status **Duplicate node key** di dashboard.
- Jangan jalankan dari folder berbeda dengan `--state-dir .`, karena tiap folder dianggap node baru.
- Di container tanpa penyimpanan permanen (Railway, Hugging Face Spaces), pakai `--ephemeral` atau arahkan `--state-dir` ke volume permanen.

## Keamanan

- `--listen :3982` membuka port ke semua interface. Kalau hanya perlu diakses dari perangkat itu sendiri, pakai `--listen 127.0.0.1:3982`. Hal yang sama berlaku untuk mode multi-port: `127.0.0.1:2222,8080`.
- Di mode `tunnel`, port hanya terbuka di Tailnet, bukan di IP publik VPS.
- Jangan commit auth key ke repo. Simpan di environment variable.

## Menjalankan di background

Termux atau VPS sederhana:

```bash
nohup ./tsrelay --mode target --listen :3982 --target 100.x.x.x > tsrelay.log 2>&1 &
```

systemd (VPS):

```ini
# /etc/systemd/system/tsrelay.service
[Unit]
Description=TSRelay
After=network-online.target

[Service]
Environment=TS_AUTHKEY=tskey-auth-xxxx
ExecStart=/usr/local/bin/tsrelay --mode tunnel --listen :3982 --target 127.0.0.1
Restart=always
User=root

[Install]
WantedBy=multi-user.target
```

```bash
sudo systemctl daemon-reload && sudo systemctl enable --now tsrelay
```

## Build dari source

Butuh Go versi terbaru (sesuai syarat `tailscale.com`).

```bash
go mod tidy
CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o tsrelay .
```

Semua arsitektur sekaligus:

```bash
chmod +x build.sh && ./build.sh   # hasil di dist/
```

## Release otomatis (GitHub Actions)

Workflow ada di `.github/workflows/release.yml`.

- Push ke `main` yang mengubah `main.go`: build semua arsitektur, lalu membuat Release baru bernama `v1.0.<nomor run>` beserta `SHA256SUMS.txt`.
- Push tag `v*`: nama tag itu yang dipakai sebagai nama Release.
- Pull request: hanya build, tanpa release.
- Run manual lewat tab Actions (*Run workflow*) juga tersedia.

## Troubleshooting

| Masalah | Penyebab dan solusi |
|---------|---------------------|
| Muncul machine baru di dashboard | State hilang atau berpindah folder. Pakai `--state-dir` tetap (default `~/.tsrelay`) |
| `Duplicate node key` | State yang sama dipakai di lebih dari satu perangkat. Hapus node di dashboard, hapus `~/.tsrelay`, login ulang per perangkat |
| `state dir ... sedang dipakai instance lain` | Masih ada proses tsrelay yang jalan. `pkill tsrelay` lalu coba lagi |
| `TS_AUTHKEY belum diisi` | Login pertama butuh auth key. `export TS_AUTHKEY=...` |
| `Authkey is set; but state is Starting. Ignoring authkey` | Normal kalau state sudah ada |
| `gagal connect target ... connection was refused` | Koneksi sampai ke mesin tujuan, tapi tidak ada service di port itu. Cek port tujuan (mis. SSH di 22, bukan 2222) dan pastikan servicenya jalan |
| `gagal connect target` (timeout) | Cek IP target, pastikan node tujuan online dan, di mode `tunnel`, tunnel di VPS jalan |
| `bind: permission denied` | Port di bawah 1024 butuh root. Pakai port 1024 ke atas, mis. `--listen :2222` |
| `... bukan alamat milik perangkat ini` | `--listen` berisi IP mesin lain. `--listen` = alamat lokal (`127.0.0.1`, `0.0.0.0`, atau kosong). IP tujuan ditulis di `--target` |
| `--target dengan port hanya untuk satu port listen` | Multi-port tidak bisa dipetakan ke satu port tujuan. Tulis `--target` tanpa port |
| `tidak ada port yang berhasil dibuka` | Semua port gagal bind (sudah terpakai atau tidak diizinkan). Lihat pesan error di baris yang sama |
| `/proc/net/route permission denied`, `SO_BINDTODEVICE` | Normal di Android/Termux, bisa diabaikan |
| DNS hostname gagal di Termux (mode `tunnel`) | Binary static Go tidak menemukan `/etc/resolv.conf`. Pakai IP di `--target` |

## Credit
| | |
|---|---|
| GitHub | [github.com/FgsiDev](https://github.com/FgsiDev) |
| Kontak | _https://whatsapp.com/channel/0029VapkSr45q08hPPPVqy26_ |
| Repo | [FgsiDev/tsrelay](https://github.com/FgsiDev/tsrelay) |

Bug atau saran fitur? Buka [Issues](https://github.com/FgsiDev/tsrelay/issues) atau kirim pull request.

## Lisensi

MIT © 2026 FgsiDev
