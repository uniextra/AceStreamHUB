package web

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	assets "acestreamhub"
	"acestreamhub/internal/channels"
	"acestreamhub/internal/config"
	"acestreamhub/internal/db"
	"acestreamhub/internal/scanner"
	"acestreamhub/internal/scheduler"
	"acestreamhub/internal/scraper"
	"acestreamhub/internal/stream"
)

func setupTestServer(t *testing.T) (*Server, *config.Manager, *channels.Store, func()) {
	t.Helper()
	dir := t.TempDir()
	cfgMgr := config.NewManager(dir)
	cfgMgr.SetupSecurity()

	dbStore, err := db.Open(dir)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	chanStore := channels.NewStore(cfgMgr, dbStore)
	hub := stream.NewHub(ctx, cfgMgr, dbStore, chanStore)
	scanSvc := scanner.NewScanner(cfgMgr, dbStore, chanStore, hub)
	sched := scheduler.NewScheduler(ctx, cfgMgr, chanStore)

	srv, err := NewServer(assets.FS, cfgMgr, dbStore, chanStore, hub, scanSvc, sched)
	if err != nil {
		cancel()
		_ = dbStore.Close()
		t.Fatalf("NewServer: %v", err)
	}

	cleanup := func() {
		cancel()
		_ = dbStore.Close()
	}
	return srv, cfgMgr, chanStore, cleanup
}

func TestWebAuthPublicAndAPIRoutes(t *testing.T) {
	srv, cfgMgr, chanStore, cleanup := setupTestServer(t)
	defer cleanup()

	chanStore.SetRawSource("s1", []scraper.RawChannel{
		{Title: "La 1", AceID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Extinf: `#EXTINF:-1,La 1`},
	})

	handler := srv.Handler()

	// 1. Protected API returns 401 when unauthenticated; dashboard redirects to /login.
	reqAPI := httptest.NewRequest(http.MethodGet, "/api/stats", nil)
	recAPI := httptest.NewRecorder()
	handler.ServeHTTP(recAPI, reqAPI)
	if recAPI.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 on unauthenticated /api/stats, got %d", recAPI.Code)
	}

	reqDash := httptest.NewRequest(http.MethodGet, "/dashboard", nil)
	recDash := httptest.NewRecorder()
	handler.ServeHTTP(recDash, reqDash)
	if recDash.Code != http.StatusFound || !strings.HasPrefix(recDash.Header().Get("Location"), "/login") {
		t.Fatalf("expected 302 to /login, got %d (%s)", recDash.Code, recDash.Header().Get("Location"))
	}

	// 2. Public feeds (/playlist.m3u, /epg.xml) bypass auth; /discover.json honours hdhr_enabled.
	reqM3U := httptest.NewRequest(http.MethodGet, "/playlist.m3u", nil)
	reqM3U.Host = "tv.local:5004"
	recM3U := httptest.NewRecorder()
	handler.ServeHTTP(recM3U, reqM3U)
	if recM3U.Code != http.StatusOK || !strings.Contains(recM3U.Body.String(), "http://tv.local:5004/auto/v") {
		t.Fatalf("unexpected /playlist.m3u response (%d): %s", recM3U.Code, recM3U.Body.String())
	}

	reqEPG := httptest.NewRequest(http.MethodGet, "/epg.xml", nil)
	recEPG := httptest.NewRecorder()
	handler.ServeHTTP(recEPG, reqEPG)
	if recEPG.Code != http.StatusOK || !strings.Contains(recEPG.Body.String(), "Programación de La 1") {
		t.Fatalf("unexpected /epg.xml response (%d): %s", recEPG.Code, recEPG.Body.String())
	}

	reqHDHR := httptest.NewRequest(http.MethodGet, "/discover.json", nil)
	recHDHR := httptest.NewRecorder()
	handler.ServeHTTP(recHDHR, reqHDHR)
	if recHDHR.Code != http.StatusNotFound {
		t.Fatalf("expected 404 when HDHR disabled, got %d", recHDHR.Code)
	}

	// 3. Initial setup via POST /login issues a valid session cookie.
	form := url.Values{
		"password":         {"secret123"},
		"confirm_password": {"secret123"},
	}
	reqSetup := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
	reqSetup.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	recSetup := httptest.NewRecorder()
	handler.ServeHTTP(recSetup, reqSetup)
	if recSetup.Code != http.StatusFound || len(recSetup.Result().Cookies()) == 0 {
		t.Fatalf("expected setup redirect with cookie, got %d", recSetup.Code)
	}
	sessionCookie := recSetup.Result().Cookies()[0]

	// 4. Enable HDHR & Hot Failover via POST /api/settings using the authenticated session cookie.
	settingsBody := bytes.NewBufferString(`{"hdhr_enabled":true,"hot_failover_enabled":true,"engine_idle_timeout":45}`)
	reqSave := httptest.NewRequest(http.MethodPost, "/api/settings", settingsBody)
	reqSave.AddCookie(sessionCookie)
	recSave := httptest.NewRecorder()
	handler.ServeHTTP(recSave, reqSave)
	if recSave.Code != http.StatusOK {
		t.Fatalf("expected 200 on /api/settings, got %d: %s", recSave.Code, recSave.Body.String())
	}
	if !cfgMgr.Load().HDHREnabled() || cfgMgr.Load().EngineIdleTimeout() != 45 {
		t.Fatalf("settings were not persisted properly: %+v", cfgMgr.Load().Settings)
	}

	recHDHR2 := httptest.NewRecorder()
	handler.ServeHTTP(recHDHR2, httptest.NewRequest(http.MethodGet, "/discover.json", nil))
	if recHDHR2.Code != http.StatusOK || !strings.Contains(recHDHR2.Body.String(), "AceStream Hub HDHR") {
		t.Fatalf("expected 200 on /discover.json once enabled, got %d", recHDHR2.Code)
	}

	// 5. Render authenticated pages and verify /api/engine/cleanup.
	for _, page := range []string{"/dashboard", "/channels", "/epg", "/sources", "/settings"} {
		reqP := httptest.NewRequest(http.MethodGet, page, nil)
		reqP.AddCookie(sessionCookie)
		recP := httptest.NewRecorder()
		handler.ServeHTTP(recP, reqP)
		if recP.Code != http.StatusOK {
			t.Fatalf("page %s returned status %d", page, recP.Code)
		}
	}

	reqCleanup := httptest.NewRequest(http.MethodPost, "/api/engine/cleanup", nil)
	reqCleanup.AddCookie(sessionCookie)
	recCleanup := httptest.NewRecorder()
	handler.ServeHTTP(recCleanup, reqCleanup)
	if recCleanup.Code != http.StatusOK {
		t.Fatalf("expected 200 on /api/engine/cleanup, got %d", recCleanup.Code)
	}
	var cleanupResp map[string]any
	if err := json.NewDecoder(recCleanup.Body).Decode(&cleanupResp); err != nil || cleanupResp["status"] != "ok" {
		t.Fatalf("unexpected cleanup response: %+v (%v)", cleanupResp, err)
	}
}
