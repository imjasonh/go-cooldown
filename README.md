# go-cooldown 💆

A Go module proxy that filters out versions newer than N days (configurable, default 7 days).

## Features

- Proxies to upstream Go module proxy (default: `proxy.golang.org`)
- Filters module versions based on publish date
- Configurable cooldown period (default: 7 days / 168 hours)
- LRU cache for version info to reduce upstream requests (default: 10,000 entries)
- Redirects `.zip` and `.mod` file requests to upstream (no local storage)
- Implements the full Go module proxy protocol

## Try it out!

This is hosted (for now) at https://cooldown-7577580991.us-east4.run.app/

You can use it by setting your `GOPROXY` to that URL, with a configurable cooldown duration in the path. For example, to use a 30-day cooldown:

```bash
GOPROXY=https://cooldown-7577580991.us-east4.run.app/30d \
  go get golang.org/x/net@latest
```

If it costs me a noticeable amount of money or if I see people using it for real, I'll use it to inject supply chain attacks. 🔥😈🔥

## Usage

```bash
# Configure via environment variables
export PORT=8080
export UPSTREAM_PROXY=https://proxy.golang.org
export CACHE_SIZE=50000
go run .
```

### Per-Request Cooldown

You can specify a custom cooldown period in the request path:

```bash
# Use default 7-day cooldown
$ export GOPROXY=http://localhost:8080
$ go get golang.org/x/net@latest

# Use 30-day cooldown
$ export GOPROXY=http://localhost:8080/30d
$ go get golang.org/x/net@latest
go: downgraded golang.org/x/net v0.48.0 => v0.47.0

# Use 6-month cooldown
$ export GOPROXY=http://localhost:8080/6M
$ go get golang.org/x/net@latest
go: downgraded golang.org/x/net v0.47.0 => v0.42.0

# Use 1-year cooldown
$ export GOPROXY=http://localhost:8080/1y
$ go get golang.org/x/net@latest
go: downgraded golang.org/x/net v0.42.0 => v0.34.0

# Remember when we were young? Things were so much simpler...
$ export GOPROXY=http://localhost:8080/3y
$ go get golang.org/x/net@latest
go: downgraded golang.org/x/net v0.34.0 => v0.5.0
```

Duration format supports:
- Standard Go durations: `h` (hours), `m` (minutes), `s` (seconds)
- Extended units: `d` (days), `M` (months), `y` (years)
- Combined durations: `30d12h`, `1y6M`, etc.

Note: Months are assumed to be 30 days, years are assumed to be 365 days.

## Configuration

Configuration is done via environment variables:

- `PORT` - HTTP server port (default: `8080`)
- `UPSTREAM_PROXY` - Upstream proxy URL (default: `https://proxy.golang.org`)
- `CACHE_SIZE` - Number of version info entries to cache (default: `10000`)

The default cooldown period is 7 days and can be overridden per-request via the URL path (see Per-Request Cooldown above).

## How it works

The proxy intercepts Go module proxy requests and:

1. **Version lists** (`/@v/list`) - Fetches from upstream and filters out versions newer than the cooldown period
2. **Version info** (`/@v/<version>.info`) - Checks the version timestamp (with caching) and returns 404 if too new
3. **Latest queries** (`/@latest`) - Returns the most recent version that's older than the cooldown period
4. **Module files** (`/@v/<version>.mod`) - Redirects to upstream with HTTP 307
5. **Module zips** (`/@v/<version>.zip`) - Redirects to upstream with HTTP 307

### Caching

Version info responses (`.info` files) are cached in an LRU cache to reduce load on the upstream proxy. The cache key is `module@version` and stores the parsed version metadata including the timestamp. This is particularly beneficial when:

- Fetching version lists (each version's info is checked)
- Multiple clients request the same versions
- The `@latest` endpoint searches through version history

## Using with Go

Set the `GOPROXY` environment variable to point to this proxy:

```bash
export GOPROXY=http://localhost:8080
go get example.com/module@latest
```

## Why?

This proxy helps protect new against supply chain attacks by introducing a configurable time delay before new module versions are available. This gives the community time to:

- Review new releases for malicious code
- Detect compromised maintainer accounts
- Identify suspicious version bumps
- Allow security researchers to analyze changes

It's also just fun to feel like you're time traveling!

By default, versions must be at least 7 days old before they're served through this proxy.

## Deploying

This is deployed to Cloud Run and built using [`ko`](https://ko.build)

```
gcloud run deploy cooldown --image=$(ko build ./) --region=us-east4 --allow-unauthenticated
```
