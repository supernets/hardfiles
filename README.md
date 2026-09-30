# HARDFILES

No logs. No tracking. No analytics. No weird anime girls on the homepage.

Upload a file, get a link, it's shredded in 24 hours.

## Features

- Drag-and-drop, clipboard paste, file picker, and curl uploads
- Optional password protection on files
- 7-pass secure file shredding on expiry
- Random GIF backgrounds from a configurable directory
- IP-based rate limiting *(uses `X-Real-IP` when the peer is a loopback or private address)*
- Streaming uploads *(no memory buffering, handles files up to 5GB)*
- Single Go binary, no database

## Quick Start

Requires Go 1.25+.

```bash
go build -o hardfiles main.go
./hardfiles
```

Or with Docker:

```bash
docker compose up -d
```

## Configuration

Edit `config.toml`:

```toml
webroot = "www"
lport = "5000"
vhost = "hardfiles.supernets.org"
filelen = 6
folder = "files"
bgfolder = "backgrounds"
max_upload_mb = 5120
ttl_hours = 24
rate_limit_per_min = 30
```

## Usage

### Browser
Visit the site, drop a file, get a link.

### curl
```bash
# Upload a file
curl -F file=@photo.png https://hardfiles.supernets.org/

# Upload with password
curl -F file=@secret.pdf -F password=hunter2 https://hardfiles.supernets.org/
```

### Bash Alias

```bash
# Add to ~/.bashrc
upload() {
    curl -F file=@$1 https://hardfiles.supernets.org/
}
```

Then just `upload /path/to/file.jpg`.

## Backgrounds

Add .gif files to the `backgrounds/` directory. Each page load picks one at random. The more chaotic, the better.

## Nginx Reverse Proxy

Hardfiles is designed to run behind nginx. Key configuration for large file uploads:

```nginx
server {
    listen 443 ssl;
    server_name hardfiles.supernets.org;

    # CRITICAL: Must match or exceed max_upload_mb in config.toml
    client_max_body_size 5120m;

    # Disable request buffering, stream directly to backend
    proxy_request_buffering off;

    # Increase timeouts for large uploads
    proxy_read_timeout 3600s;
    proxy_send_timeout 3600s;
    proxy_connect_timeout 60s;

    # Increase header buffer for large multipart boundaries
    proxy_buffer_size 16k;
    proxy_buffers 4 32k;

    location / {
        proxy_pass http://127.0.0.1:5000;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
        proxy_http_version 1.1;
    }

    ssl_certificate /etc/letsencrypt/live/hardfiles.supernets.org/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/hardfiles.supernets.org/privkey.pem;
}
```

**Important nginx settings:**
- `client_max_body_size 5120m`: must match `max_upload_mb` in config.toml, or nginx will reject uploads before they reach hardfiles
- `proxy_request_buffering off`: prevents nginx from buffering the upload to disk before forwarding *(critical for memory-limited servers)*
- `proxy_read_timeout 3600s`: 1 hour timeout for large uploads on slow connections

## Docker

```bash
# Build and run
docker compose up -d --build

# Volumes mounted:
# ./files:/app/files              uploaded files (shredded after ttl_hours)
# ./backgrounds:/app/backgrounds  GIF backgrounds
# ./config.toml:/app/config.toml  configuration
```

The container listens on `127.0.0.1:5000` only, so it must be fronted by a reverse proxy.

## Tests

```bash
go test ./...
```

## Security

- Files are shredded with a 7-pass random overwrite plus a zero pass before deletion *(effective on HDD, ceremonial on SSD, use dm-crypt/LUKS for SSD)*
- Path traversal prevention on all routes
- Upload size limits enforced at the HTTP level
- Password-protected files use bcrypt hashing
- MIME type allowlist for inline serving *(images, PDFs, text, audio, video)*, HTML/SVG/JS are forced to download to prevent stored XSS
- Content-Security-Policy headers on all HTML responses
- No file listing or directory browsing

## Credits

- **delorean** for developing hardfiles
- **hgw** for branding the product
- **acidvegas** for funding the project

## License

ISC, see [LICENSE](LICENSE)
