package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/chainguard-dev/clog"
	lru "github.com/hashicorp/golang-lru/v2"
)

func TestProxy(t *testing.T) {
	ctx := context.Background()
	log := clog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	}))
	ctx = clog.WithLogger(ctx, log)

	// Create a mock upstream server
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/example.com/module/@v/list":
			// Return a list of versions
			fmt.Fprintln(w, "v1.0.0")
			fmt.Fprintln(w, "v1.1.0")
			fmt.Fprintln(w, "v1.2.0")
			fmt.Fprintln(w, "v2.0.0")

		case "/example.com/module/@v/v1.0.0.info":
			// Old version (30 days ago)
			info := VersionInfo{
				Version: "v1.0.0",
				Time:    time.Now().Add(-30 * 24 * time.Hour),
			}
			json.NewEncoder(w).Encode(info)

		case "/example.com/module/@v/v1.1.0.info":
			// Old version (14 days ago)
			info := VersionInfo{
				Version: "v1.1.0",
				Time:    time.Now().Add(-14 * 24 * time.Hour),
			}
			json.NewEncoder(w).Encode(info)

		case "/example.com/module/@v/v1.2.0.info":
			// Recent version (5 days ago) - should be filtered with 7 day cooldown
			info := VersionInfo{
				Version: "v1.2.0",
				Time:    time.Now().Add(-5 * 24 * time.Hour),
			}
			json.NewEncoder(w).Encode(info)

		case "/example.com/module/@v/v2.0.0.info":
			// Very recent version (1 day ago) - should be filtered
			info := VersionInfo{
				Version: "v2.0.0",
				Time:    time.Now().Add(-1 * 24 * time.Hour),
			}
			json.NewEncoder(w).Encode(info)

		case "/example.com/module/@v/v1.0.0.zip":
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("fake zip content"))

		case "/example.com/module/@v/v1.0.0.mod":
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("module example.com/module\n\ngo 1.21\n"))

		case "/example.com/module/@latest":
			// Latest is v2.0.0 which is too recent
			info := VersionInfo{
				Version: "v2.0.0",
				Time:    time.Now().Add(-1 * 24 * time.Hour),
			}
			json.NewEncoder(w).Encode(info)

		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	cache, err := lru.New[string, *VersionInfo](100)
	if err != nil {
		t.Fatal(err)
	}

	proxy := &Proxy{
		upstream:        upstream.URL,
		client:          &http.Client{Timeout: 30 * time.Second},
		cache:           cache,
		defaultCooldown: 7 * 24 * time.Hour,
	}

	for _, tt := range []struct {
		desc         string
		path         string
		wantStatus   int
		wantContains string
		wantLines    int
	}{
		{
			desc:         "list filters recent versions",
			path:         "/example.com/module/@v/list",
			wantStatus:   http.StatusOK,
			wantContains: "v1.0.0",
			wantLines:    2, // v1.0.0 and v1.1.0 only
		},
		{
			desc:         "old version info is allowed",
			path:         "/example.com/module/@v/v1.0.0.info",
			wantStatus:   http.StatusOK,
			wantContains: "v1.0.0",
		},
		{
			desc:       "recent version info is filtered",
			path:       "/example.com/module/@v/v2.0.0.info",
			wantStatus: http.StatusNotFound,
		},
		{
			desc:       "latest returns older version when latest is too new",
			path:       "/example.com/module/@latest",
			wantStatus: http.StatusOK,
			// Should return v1.1.0 since v2.0.0 and v1.2.0 are too recent
			wantContains: "v1.1.0",
		},
	} {
		t.Run(tt.desc, func(t *testing.T) {
			req := httptest.NewRequest("GET", tt.path, nil)
			req = req.WithContext(ctx)
			w := httptest.NewRecorder()

			proxy.ServeHTTP(w, req)

			if w.Code != tt.wantStatus {
				t.Errorf("status: got %d, want %d", w.Code, tt.wantStatus)
			}

			body := w.Body.String()
			if tt.wantContains != "" && !strings.Contains(body, tt.wantContains) {
				t.Errorf("body should contain %q, got: %s", tt.wantContains, body)
			}

			if tt.wantLines > 0 {
				lines := strings.Split(strings.TrimSpace(body), "\n")
				if len(lines) != tt.wantLines {
					t.Errorf("expected %d lines, got %d: %v", tt.wantLines, len(lines), lines)
				}
			}
		})
	}
}

func TestProxyRedirects(t *testing.T) {
	ctx := context.Background()
	log := clog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	}))
	ctx = clog.WithLogger(ctx, log)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("upstream content"))
	}))
	defer upstream.Close()

	cache, err := lru.New[string, *VersionInfo](100)
	if err != nil {
		t.Fatal(err)
	}

	proxy := &Proxy{
		upstream:        upstream.URL,
		client:          &http.Client{Timeout: 30 * time.Second},
		cache:           cache,
		defaultCooldown: 7 * 24 * time.Hour,
	}

	for _, tt := range []struct {
		desc string
		path string
	}{
		{
			desc: "zip files are redirected",
			path: "/example.com/module/@v/v1.0.0.zip",
		},
		{
			desc: "mod files are redirected",
			path: "/example.com/module/@v/v1.0.0.mod",
		},
	} {
		t.Run(tt.desc, func(t *testing.T) {
			req := httptest.NewRequest("GET", tt.path, nil)
			req = req.WithContext(ctx)
			w := httptest.NewRecorder()

			proxy.ServeHTTP(w, req)

			if w.Code != http.StatusTemporaryRedirect {
				t.Errorf("status: got %d, want %d", w.Code, http.StatusTemporaryRedirect)
			}

			location := w.Header().Get("Location")
			expectedLocation := upstream.URL + tt.path
			if location != expectedLocation {
				t.Errorf("Location header: got %q, want %q", location, expectedLocation)
			}
		})
	}
}

func TestCooldownPeriods(t *testing.T) {
	ctx := context.Background()
	log := clog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	}))
	ctx = clog.WithLogger(ctx, log)

	for _, tt := range []struct {
		desc            string
		cooldownDays    int
		versionAgeDays  int
		shouldBeAllowed bool
	}{
		{
			desc:            "version exactly at cooldown boundary is allowed",
			cooldownDays:    7,
			versionAgeDays:  7,
			shouldBeAllowed: true,
		},
		{
			desc:            "version older than cooldown is allowed",
			cooldownDays:    7,
			versionAgeDays:  8,
			shouldBeAllowed: true,
		},
		{
			desc:            "version newer than cooldown is filtered",
			cooldownDays:    7,
			versionAgeDays:  6,
			shouldBeAllowed: false,
		},
		{
			desc:            "version with 1 day cooldown",
			cooldownDays:    1,
			versionAgeDays:  2,
			shouldBeAllowed: true,
		},
		{
			desc:            "version with 30 day cooldown",
			cooldownDays:    30,
			versionAgeDays:  29,
			shouldBeAllowed: false,
		},
	} {
		t.Run(tt.desc, func(t *testing.T) {
			versionTime := time.Now().Add(-time.Duration(tt.versionAgeDays) * 24 * time.Hour)

			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, ".info") {
					info := VersionInfo{
						Version: "v1.0.0",
						Time:    versionTime,
					}
					json.NewEncoder(w).Encode(info)
				}
			}))
			defer upstream.Close()

			cache, err := lru.New[string, *VersionInfo](100)
			if err != nil {
				t.Fatal(err)
			}

			proxy := &Proxy{
				upstream:        upstream.URL,
				client:          &http.Client{Timeout: 30 * time.Second},
				cache:           cache,
				defaultCooldown: time.Duration(tt.cooldownDays) * 24 * time.Hour,
			}

			req := httptest.NewRequest("GET", "/example.com/module/@v/v1.0.0.info", nil)
			req = req.WithContext(ctx)
			w := httptest.NewRecorder()

			proxy.ServeHTTP(w, req)

			if tt.shouldBeAllowed {
				if w.Code != http.StatusOK {
					t.Errorf("expected version to be allowed, got status %d", w.Code)
				}
			} else {
				if w.Code != http.StatusNotFound {
					t.Errorf("expected version to be filtered, got status %d", w.Code)
				}
			}
		})
	}
}
