# Penninn Midlun

A lightweight, self-hosted flipbook generator. Upload PowerPoint or PDF files and get beautiful 3D page-curl flipbooks hosted as SEO-optimized webpages, or embeddable in any site via iframe.

**[See a live example](https://flipbook.dev.nextdigital.is)**

## Features

- **Upload & convert** PowerPoint (.pptx, .ppt) and PDF files to interactive flipbooks
- **Import from Google Slides** via public share URL
- **3D page-curl viewer** powered by [StPageFlip](https://github.com/Nodlik/StPageFlip) with keyboard navigation, fullscreen, and deep-linking
- **SEO-optimized pages** with full slide text rendered as hidden semantic HTML, Open Graph tags, Twitter Cards, JSON-LD structured data, and canonical URLs
- **Embeddable** via iframe with a single line of HTML, or host directly as standalone pages
- **Grid view** for browsing all slides at a glance
- **Full-text search** across slide content
- **MCP server** for creating flipbooks from AI agent workflows (Claude Code, Cowork, etc.)
- **Admin dashboard** with upload progress tracking, thumbnail previews, and embed code generation
- **Password-protected admin** with bcrypt-hashed credentials and session-based auth
- **REST API** with required API key authentication
- **Background conversion** with real-time progress updates
- **No build tools required** — plain Go templates, vanilla JS, no npm/webpack

## Tech Stack

| Component | Technology |
|-----------|-----------|
| Backend | Go + [chi](https://github.com/go-chi/chi) router |
| Database | MongoDB Atlas |
| Conversion | LibreOffice headless (PPTX/PPT to PDF) + pdftoppm/poppler (PDF to PNG) |
| Text extraction | pdftotext (poppler) for search + SEO |
| Viewer | [StPageFlip](https://github.com/Nodlik/StPageFlip) (vendored, MIT license) |
| Frontend | Server-rendered Go templates, vanilla CSS/JS |

## Prerequisites

- **Go** 1.21+
- **LibreOffice** (for PowerPoint conversion)
- **Poppler** (provides `pdftoppm` and `pdftotext`)
- **MongoDB** (Atlas or local)

### macOS

```bash
brew install poppler
brew install --cask libreoffice
```

### Ubuntu/Debian

```bash
sudo apt install poppler-utils libreoffice-impress
```

## Quick Start

```bash
# Clone the repo
git clone https://github.com/metavert-io/flipbook.git
cd flipbook

# Copy and edit config
cp config.example.yaml config.dev.yaml
# Edit config.dev.yaml with your MongoDB URI

# Download frontend dependencies
make setup

# Set an admin password
make set-password

# Start the server
make run
```

The server starts at [http://localhost:8080](http://localhost:8080).

## Configuration

Flipbook loads configuration from (in order of priority):

1. Environment variables (prefixed with `FLIPBOOK_`)
2. `config.dev.yaml` (for development, gitignored)
3. `config.yaml` (for production)

See [`config.example.yaml`](config.example.yaml) for all available options.

Key settings:

| Setting | Env Variable | Default | Description |
|---------|-------------|---------|-------------|
| `port` | `FLIPBOOK_PORT` | `8080` | Server port |
| `base_url` | `FLIPBOOK_BASE_URL` | `http://localhost:8080` | Public URL |
| `mongo_uri` | `FLIPBOOK_MONGO_URI` | — | MongoDB connection string |
| `session_secret` | `FLIPBOOK_SESSION_SECRET` | auto-generated | Session signing key |
| `api_key` | `FLIPBOOK_API_KEY` | auto-generated | Bearer token for API/MCP auth |

## Usage

### Admin settings

The **Settings** section in the admin panel (Team → Settings, `/admin/settings`) manages administrators and instance configuration:

- **Add accounts** — create named admins, editors, publishers, or readers with a name, email, and password directly, without an invitation link.
- **Passwords** — rotate the primary `admin` login password from the UI (no CLI needed) and reset any local member's password; either action signs out that account's existing sessions.
- **Instance settings** — site name (shown in the admin navigation), session duration, and the per-minute sign-in attempt limit. These are stored in the database and apply immediately; server-level options remain in the config file and require a restart.

The `set-password` CLI command still works and is equivalent to rotating the primary administrator password.

### Upload a file

1. Go to **http://localhost:8080/admin**
2. Log in with your admin password
3. Click **Upload** and drag in a `.pptx`, `.ppt`, or `.pdf` file
4. Watch the real-time progress tracker as it converts
5. Click **View Flipbook** when ready

### Import from Google Slides

1. In Google Slides, click **Share** and set access to "Anyone with the link"
2. Copy the URL
3. In the admin, switch to the **Import from URL** tab
4. Paste the Google Slides URL and click **Import & Convert**

### Viewing flipbooks

Each flipbook gets its own SEO-optimized page at `/v/{slug}`. These are standalone pages suitable for direct linking, sharing on social media, or indexing by search engines. The page includes:

- Interactive 3D page-curl viewer
- Full slide text rendered as hidden semantic HTML for search engine crawlers
- Open Graph and Twitter Card meta tags with the first slide as the preview image
- JSON-LD structured data (`PresentationDigitalDocument`)
- Canonical URL for proper indexing

### Embedding in a webpage

If you prefer to embed a flipbook in an existing page, use the iframe embed code from the admin detail page:

```html
<iframe src="https://your-domain.com/embed/my-presentation"
        width="800" height="600" frameborder="0" allowfullscreen
        style="border:none;border-radius:8px;box-shadow:0 2px 8px rgba(0,0,0,0.1);">
</iframe>
```

The `/embed/{slug}` endpoint replaces the site-wide `X-Frame-Options: SAMEORIGIN` with `Content-Security-Policy: frame-ancestors`, taken from the publication's **Embed behavior** settings:

- **Allow public embedding** must be on, or anonymous readers get "Embedding is disabled". New publications start with it on; the admin preview works either way because staff bypass the check.
- **Allowed frame-ancestor domains** empty means any site may embed. A bare `example.com` also covers `www.example.com`; use `*.example.com` for every subdomain, or `https://host` for one exact origin.
- The publication must be published with public or unlisted visibility.

Error pages under `/embed/` use the same policy, so a blocked embed shows its reason inside the iframe instead of the browser's "refused to connect".

### API

All API routes require a bearer token. The API key is logged at server startup (auto-generated if not set in config).

```bash
# List all flipbooks
curl -H "Authorization: Bearer YOUR_API_KEY" http://localhost:8080/api/flipbooks

# Upload a file
curl -H "Authorization: Bearer YOUR_API_KEY" -X POST -F "file=@deck.pptx" http://localhost:8080/api/flipbooks

# Import from Google Slides
curl -H "Authorization: Bearer YOUR_API_KEY" -X POST \
  -F "url=https://docs.google.com/presentation/d/PRES_ID/edit" \
  http://localhost:8080/api/flipbooks/import

# Get flipbook details
curl -H "Authorization: Bearer YOUR_API_KEY" http://localhost:8080/api/flipbooks/{id}

# Check conversion status (no auth required)
curl http://localhost:8080/api/flipbooks/{id}/status

# Delete a flipbook
curl -H "Authorization: Bearer YOUR_API_KEY" -X DELETE http://localhost:8080/api/flipbooks/{id}
```

### MCP (AI Agent Integration)

The built-in MCP server lets AI agents create and manage flipbooks programmatically. It communicates via JSON-RPC 2.0 over stdin/stdout and authenticates to the API using the same API key from config.

```bash
# Start the MCP server (the web server must be running separately)
./flipbook mcp
```

Configure in Claude Code (`~/.claude/settings.json`) or any MCP-compatible tool:

```json
{
  "mcpServers": {
    "flipbook": {
      "command": "/path/to/flipbook",
      "args": ["mcp"]
    }
  }
}
```

**Available MCP tools:**

| Tool | Description |
|------|-------------|
| `list_flipbooks` | List all flipbooks with status and URLs |
| `create_flipbook` | Upload a file (base64), wait for conversion |
| `import_google_slides` | Import from Google Slides URL, wait for conversion |
| `get_flipbook` | Get flipbook details, page URLs, embed code |
| `get_flipbook_status` | Check conversion status |
| `delete_flipbook` | Delete a flipbook and its files |

## Project Structure

```
flipbook/
├── main.go                          # Entry point, routing, CLI commands
├── internal/
│   ├── auth/auth.go                 # Password auth + session management
│   ├── config/config.go             # YAML + env config loading
│   ├── converter/                   # PPTX→PDF→PNG pipeline + text extraction
│   ├── database/database.go         # MongoDB operations
│   ├── handlers/                    # HTTP handlers (admin, API, viewer, embed)
│   ├── mcp/server.go                # MCP server (JSON-RPC 2.0 over stdio)
│   ├── models/flipbook.go           # Data models
│   ├── storage/storage.go           # Filesystem storage
│   └── worker/worker.go             # Background conversion queue
├── web/
│   ├── templates/                   # Go HTML templates
│   └── static/                      # CSS, JS, vendored libraries
├── config.example.yaml              # Example configuration
└── data/                            # Runtime data (gitignored)
```

## License

[MIT](LICENSE) - Copyright (c) 2026 Metavert LLC
