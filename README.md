# TSRelay

Relay TCP sederhana di atas Tailscale (`tsnet`). Dipakai untuk mengakses service di VPS lewat jaringan Tailscale, bukan lewat IP publik VPS. Satu binary static, jalan di Termux (32/64-bit) dan VPS (x86/x86_64/ARM).

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

Port di `--target` bersifat opsional. Kalau tidak ditulis, port yang sama dengan `--listen` dipakai.

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

## Flag

| Flag | Default | Keterangan |
|------|---------|------------|
| `--mode` | `tunnel` | `tunnel` atau `target` |
| `--listen` | `:3982` | Alamat atau port listen (`:3982` atau `3982`) |
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

- `--listen :3982` membuka port ke semua interface. Kalau hanya perlu diakses dari perangkat itu sendiri, pakai `--listen 127.0.0.1:3982`.
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

- Push ke `main`: build semua arsitektur, lalu membuat Release baru bernama `v1.0.<nomor run>` beserta `SHA256SUMS.txt`.
- Push tag `v*`: nama tag itu yang dipakai sebagai nama Release.
- Pull request: hanya build, tanpa release.

## Troubleshooting

| Masalah | Penyebab dan solusi |
|---------|---------------------|
| Muncul machine baru di dashboard | State hilang atau berpindah folder. Pakai `--state-dir` tetap (default `~/.tsrelay`) |
| `Duplicate node key` | State yang sama dipakai di lebih dari satu perangkat. Hapus node di dashboard, hapus `~/.tsrelay`, login ulang per perangkat |
| `state dir ... sedang dipakai instance lain` | Masih ada proses tsrelay yang jalan. `pkill tsrelay` lalu coba lagi |
| `TS_AUTHKEY belum diisi` | Login pertama butuh auth key. `export TS_AUTHKEY=...` |
| `Authkey is set; but state is Starting. Ignoring authkey` | Normal kalau state sudah ada |
| `gagal connect target` | Cek IP/port target, pastikan tunnel di VPS jalan dan node-nya online |
| `/proc/net/route permission denied`, `SO_BINDTODEVICE` | Normal di Android/Termux, bisa diabaikan |
| DNS hostname gagal di Termux (mode `tunnel`) | Binary static Go tidak menemukan `/etc/resolv.conf`. Pakai IP di `--target` |
| `Cannot autolaunch D-Bus without X11 $DISPLAY` | Dari program lain di Termux, tidak terkait TSRelay |

## Lisensi

Tentukan sendiri (MIT, dll.).
