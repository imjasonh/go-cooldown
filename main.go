package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/chainguard-dev/clog"
	lru "github.com/hashicorp/golang-lru/v2"
	"github.com/sethvargo/go-envconfig"
)

var cfg = envconfig.MustProcess(context.Background(), &(struct {
	Port            int    `env:"PORT,default=8080"`
	UpstreamProxy   string `env:"UPSTREAM_PROXY,default=https://proxy.golang.org"`
	CacheSize       int    `env:"CACHE_SIZE,default=10000"`
	DefaultCooldown string `env:"DEFAULT_COOLDOWN,default=7d"`
}{}))

// parseDuration extends time.ParseDuration to support days (d), months (M), and years (y).
// Assumes: 1 day = 24h, 1 month = 30 days, 1 year = 365 days
func parseDuration(s string) (time.Duration, error) {
	// Try standard parsing first
	if d, err := time.ParseDuration(s); err == nil {
		return d, nil
	}

	// Handle custom units: d, M, y
	var total time.Duration
	var num strings.Builder

	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= '0' && c <= '9' || c == '.' {
			num.WriteByte(c)
		} else {
			if num.Len() == 0 {
				return 0, fmt.Errorf("invalid duration: %s", s)
			}

			val := 0.0
			if _, err := fmt.Sscanf(num.String(), "%f", &val); err != nil {
				return 0, fmt.Errorf("invalid duration: %s", s)
			}

			var unit time.Duration
			switch c {
			case 'd':
				unit = 24 * time.Hour
			case 'M':
				unit = 30 * 24 * time.Hour
			case 'y':
				unit = 365 * 24 * time.Hour
			default:
				// Try parsing the rest as a standard duration
				remainder := num.String() + s[i:]
				d, err := time.ParseDuration(remainder)
				if err != nil {
					return 0, fmt.Errorf("invalid duration: %s", s)
				}
				return total + d, nil
			}

			total += time.Duration(val * float64(unit))
			num.Reset()
		}
	}

	if num.Len() > 0 {
		return 0, fmt.Errorf("invalid duration: %s", s)
	}

	return total, nil
}

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})))
	ctx := context.Background()
	log := clog.FromContext(ctx)

	log.InfoContext(ctx, "starting go-cooldown proxy",
		"port", cfg.Port,
		"upstream", cfg.UpstreamProxy,
	)

	cache, err := lru.New[string, *VersionInfo](cfg.CacheSize)
	if err != nil {
		log.FatalContext(ctx, "failed to create cache", "error", err)
	}

	defaultCooldown, err := parseDuration(cfg.DefaultCooldown)
	if err != nil {
		log.FatalContext(ctx, "invalid default cooldown duration", "error", err)
	}

	proxy := &Proxy{
		upstream:        cfg.UpstreamProxy,
		client:          &http.Client{Timeout: 30 * time.Second},
		cache:           cache,
		defaultCooldown: defaultCooldown,
	}

	http.HandleFunc("/", proxy.ServeHTTP)

	addr := fmt.Sprintf(":%d", cfg.Port)
	log.InfoContext(ctx, "listening", "addr", addr)
	if err := http.ListenAndServe(addr, nil); err != nil {
		log.FatalContext(ctx, "server failed", "error", err)
	}
}

type Proxy struct {
	upstream        string
	client          *http.Client
	cache           *lru.Cache[string, *VersionInfo]
	defaultCooldown time.Duration
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	log := clog.FromContext(ctx)
	log.InfoContext(ctx, "request", "path", r.URL.Path)

	// Try to extract cooldown from first path segment
	var cooldown time.Duration
	var cooldownStr string

	pathParts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/"), "/", 2)
	if len(pathParts) >= 2 {
		// Try to parse first segment as duration
		if d, err := parseDuration(pathParts[0]); err == nil {
			// Valid duration found
			cooldown = d
			cooldownStr = pathParts[0]
		}
	}

	// If no valid duration prefix, use default
	if cooldownStr == "" {
		cooldown = p.defaultCooldown
	}

	// Parse the path to determine the request type
	// Go proxy paths look like:
	// /<module>/@v/list
	// /<module>/@v/<version>.info
	// /<module>/@v/<version>.mod
	// /<module>/@v/<version>.zip
	// /<module>/@latest

	// Strip the cooldown prefix from the path if present
	path := r.URL.Path
	if cooldownStr != "" {
		cooldownPrefix := "/" + cooldownStr + "/"
		path = strings.TrimPrefix(r.URL.Path, cooldownPrefix)
		if !strings.HasPrefix(path, "/") {
			path = "/" + path
		}
	}

	// Check for @latest first
	if strings.HasSuffix(path, "/@latest") {
		modulePath := strings.TrimSuffix(strings.TrimPrefix(path, "/"), "/@latest")
		log = log.With("module", modulePath)
		p.handleLatest(ctx, cooldown, w, r, modulePath)
		return
	}

	parts := strings.Split(strings.TrimPrefix(path, "/"), "/@v/")
	if len(parts) != 2 {
		// Invalid path, proxy directly
		p.proxyRequest(ctx, w, path)
		return
	}

	modulePath := parts[0]
	versionPath := parts[1]

	log = log.With("module", modulePath, "version_path", versionPath)

	// Handle different request types
	switch {
	case versionPath == "list":
		// Filter version list
		p.handleList(ctx, cooldown, w, modulePath)
	case strings.HasSuffix(versionPath, ".info"):
		// Check if version is within cooldown
		version := strings.TrimSuffix(versionPath, ".info")
		p.handleInfo(ctx, cooldown, w, modulePath, version)
	case strings.HasSuffix(versionPath, ".mod"), strings.HasSuffix(versionPath, ".zip"):
		// Redirect to upstream
		p.redirectToUpstream(ctx, w, path)
	default:
		// Unknown request type, proxy directly
		p.proxyRequest(ctx, w, path)
	}
}

func (p *Proxy) handleList(ctx context.Context, cooldown time.Duration, w http.ResponseWriter, modulePath string) {
	log := clog.FromContext(ctx)

	// Fetch the version list from upstream
	upstreamURL := fmt.Sprintf("%s/%s/@v/list", p.upstream, modulePath)
	resp, err := p.client.Get(upstreamURL)
	if err != nil {
		log.ErrorContext(ctx, "failed to fetch version list", "error", err)
		http.Error(w, "failed to fetch version list", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		log.WarnContext(ctx, "upstream returned non-200", "status", resp.StatusCode)
		w.WriteHeader(resp.StatusCode)
		io.Copy(w, resp.Body)
		return
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		log.ErrorContext(ctx, "failed to read response", "error", err)
		http.Error(w, "failed to read response", http.StatusInternalServerError)
		return
	}

	versions := strings.Split(strings.TrimSpace(string(body)), "\n")
	filteredVersions := []string{}

	cutoffTime := time.Now().Add(-cooldown)

	for _, version := range versions {
		if version == "" {
			continue
		}

		// Fetch .info for each version to check timestamp (with caching)
		info, err := p.fetchVersionInfo(ctx, modulePath, version)
		if err != nil {
			log.WarnContext(ctx, "failed to fetch version info, skipping", "version", version, "error", err)
			continue
		}

		if info.Time.Before(cutoffTime) || info.Time.Equal(cutoffTime) {
			filteredVersions = append(filteredVersions, version)
			log.DebugContext(ctx, "version included", "version", version, "time", info.Time)
		} else {
			log.InfoContext(ctx, "version filtered out", "version", version, "time", info.Time, "cutoff", cutoffTime)
		}
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	for _, v := range filteredVersions {
		fmt.Fprintf(w, "%s\n", v)
	}
}

func (p *Proxy) handleInfo(ctx context.Context, cooldown time.Duration, w http.ResponseWriter, modulePath, version string) {
	log := clog.FromContext(ctx)

	// Fetch .info from upstream (with caching)
	info, err := p.fetchVersionInfo(ctx, modulePath, version)
	if err != nil {
		log.ErrorContext(ctx, "failed to fetch version info", "error", err)
		http.Error(w, "failed to fetch version info", http.StatusBadGateway)
		return
	}

	cutoffTime := time.Now().Add(-cooldown)
	if info.Time.After(cutoffTime) {
		log.InfoContext(ctx, "version too new", "version", version, "time", info.Time, "cutoff", cutoffTime)
		http.Error(w, "version not found", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(info)
}

func (p *Proxy) handleLatest(ctx context.Context, cooldown time.Duration, w http.ResponseWriter, r *http.Request, modulePath string) {
	log := clog.FromContext(ctx)

	// Fetch @latest from upstream
	latestURL := fmt.Sprintf("%s/%s/@latest", p.upstream, modulePath)
	resp, err := p.client.Get(latestURL)
	if err != nil {
		log.ErrorContext(ctx, "failed to fetch latest", "error", err)
		http.Error(w, "failed to fetch latest", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		log.WarnContext(ctx, "upstream returned non-200", "status", resp.StatusCode)
		w.WriteHeader(resp.StatusCode)
		io.Copy(w, resp.Body)
		return
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		log.ErrorContext(ctx, "failed to read response", "error", err)
		http.Error(w, "failed to read response", http.StatusInternalServerError)
		return
	}

	var info VersionInfo
	if err := json.Unmarshal(body, &info); err != nil {
		log.ErrorContext(ctx, "failed to parse latest info", "error", err)
		http.Error(w, "failed to parse latest info", http.StatusInternalServerError)
		return
	}

	cutoffTime := time.Now().Add(-cooldown)
	if info.Time.After(cutoffTime) {
		// Latest is too new, need to find the most recent version that's old enough
		log.InfoContext(ctx, "latest version too new, searching for older version", "latest_time", info.Time, "cutoff", cutoffTime)

		// Fetch the version list and find the newest version within cooldown
		listURL := fmt.Sprintf("%s/%s/@v/list", p.upstream, modulePath)
		listResp, err := p.client.Get(listURL)
		if err != nil {
			log.ErrorContext(ctx, "failed to fetch version list", "error", err)
			http.Error(w, "failed to fetch version list", http.StatusBadGateway)
			return
		}
		defer listResp.Body.Close()

		if listResp.StatusCode != http.StatusOK {
			log.WarnContext(ctx, "upstream list returned non-200", "status", listResp.StatusCode)
			w.WriteHeader(listResp.StatusCode)
			io.Copy(w, listResp.Body)
			return
		}

		listBody, err := io.ReadAll(listResp.Body)
		if err != nil {
			log.ErrorContext(ctx, "failed to read list response", "error", err)
			http.Error(w, "failed to read list response", http.StatusInternalServerError)
			return
		}

		versions := strings.Split(strings.TrimSpace(string(listBody)), "\n")

		var latestOldEnough *VersionInfo
		for i := len(versions) - 1; i >= 0; i-- {
			version := strings.TrimSpace(versions[i])
			if version == "" {
				continue
			}

			versionInfo, err := p.fetchVersionInfo(ctx, modulePath, version)
			if err != nil {
				log.WarnContext(ctx, "failed to fetch version info", "version", version, "error", err)
				continue
			}

			if versionInfo.Time.Before(cutoffTime) || versionInfo.Time.Equal(cutoffTime) {
				latestOldEnough = versionInfo
				break
			}
		}

		if latestOldEnough == nil {
			log.InfoContext(ctx, "no versions found within cooldown period")
			http.Error(w, "no versions available", http.StatusNotFound)
			return
		}

		info = *latestOldEnough
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(info)
}

func (p *Proxy) redirectToUpstream(ctx context.Context, w http.ResponseWriter, path string) {
	log := clog.FromContext(ctx)

	upstreamURL := p.upstream + path
	log.InfoContext(ctx, "redirecting to upstream", "url", upstreamURL)

	w.Header().Set("Location", upstreamURL)
	w.WriteHeader(http.StatusTemporaryRedirect)
}

func (p *Proxy) proxyRequest(ctx context.Context, w http.ResponseWriter, path string) {
	log := clog.FromContext(ctx)

	upstreamURL := p.upstream + path
	log.InfoContext(ctx, "proxying request", "url", upstreamURL)

	resp, err := p.client.Get(upstreamURL)
	if err != nil {
		log.ErrorContext(ctx, "failed to proxy request", "error", err)
		http.Error(w, "failed to proxy request", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	maps.Copy(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

type VersionInfo struct {
	Version string    `json:"Version"`
	Time    time.Time `json:"Time"`
}

// fetchVersionInfo fetches version info with caching
func (p *Proxy) fetchVersionInfo(ctx context.Context, modulePath, version string) (*VersionInfo, error) {
	log := clog.FromContext(ctx)
	cacheKey := fmt.Sprintf("%s@%s", modulePath, version)

	// Check cache first
	if cached, ok := p.cache.Get(cacheKey); ok {
		log.DebugContext(ctx, "cache hit", "module", modulePath, "version", version)
		return cached, nil
	}

	log.DebugContext(ctx, "cache miss", "module", modulePath, "version", version)

	// Fetch from upstream
	infoURL := fmt.Sprintf("%s/%s/@v/%s.info", p.upstream, modulePath, version)
	resp, err := p.client.Get(infoURL)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("upstream returned status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	var info VersionInfo
	if err := json.Unmarshal(body, &info); err != nil {
		return nil, fmt.Errorf("failed to parse: %w", err)
	}

	// Store in cache
	p.cache.Add(cacheKey, &info)

	return &info, nil
}
