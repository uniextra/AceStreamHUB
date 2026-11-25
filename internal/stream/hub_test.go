package stream

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"acestreamhub/internal/channels"
	"acestreamhub/internal/config"
	"acestreamhub/internal/db"
)

func TestHubMultiplexAndHotFailover(t *testing.T) {
	id1 := "1111111111111111111111111111111111111111"
	id2 := "2222222222222222222222222222222222222222"

	var stopCalls int
	var mu sync.Mutex
	proxySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/cmd" && r.URL.Query().Get("method") == "stop" {
			mu.Lock()
			stopCalls++
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
			return
		}
		if r.URL.Path == "/ace/stat" {
			_, _ = w.Write([]byte(`{"response":{"peers":12}}`))
			return
		}
		id := r.URL.Query().Get("id")
		flusher, _ := w.(http.Flusher)
		w.WriteHeader(http.StatusOK)
		switch id {
		case id1:
			// Emit first TS segment then abruptly drop upstream to trigger Hot Failover.
			_, _ = w.Write(append([]byte{0x47}, bytes.Repeat([]byte{'A'}, 187)...))
			if flusher != nil {
				flusher.Flush()
			}
		case id2:
			// Emit misaligned bytes followed by MPEG-TS 0x47 sync byte and payload from backup source.
			_, _ = w.Write(append([]byte{0xFF, 0xFF, 0x47}, bytes.Repeat([]byte{'B'}, 187)...))
			if flusher != nil {
				flusher.Flush()
			}
			time.Sleep(150 * time.Millisecond)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer proxySrv.Close()

	t.Setenv("ACESTREAM_URL", proxySrv.URL)
	t.Setenv("ENGINE_URL", proxySrv.URL+"/engine")

	dir := t.TempDir()
	cfgMgr := config.NewManager(dir)
	dbStore, err := db.Open(dir)
	if err != nil {
		t.Fatalf("db open failed: %v", err)
	}
	defer dbStore.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	chanStore := channels.NewStore(cfgMgr, dbStore)
	hub := NewHub(ctx, cfgMgr, dbStore, chanStore)
	hub.RegisterEngineSession(id1, proxySrv.URL+"/cmd")

	ch := channels.Channel{
		GuideNumber: "1001",
		GuideName:   "Test TV",
		AceIDs:      []string{id1, id2},
	}

	sub1, err := hub.Subscribe(ch, "c1", "10.0.0.1", "VLC")
	if err != nil {
		t.Fatalf("Subscribe client 1 failed: %v", err)
	}
	defer sub1.Release()

	sub2, err := hub.Subscribe(ch, "c2", "10.0.0.2", "Kodi")
	if err != nil {
		t.Fatalf("Subscribe client 2 failed: %v", err)
	}
	defer sub2.Release()

	collect := func(sub *ClientSubscription) []byte {
		var out []byte
		deadline := time.After(2 * time.Second)
		for {
			select {
			case <-deadline:
				return out
			case chunk, open := <-sub.Ch:
				if !open {
					return out
				}
				out = append(out, chunk...)
				if bytes.Contains(out, []byte("BBB")) {
					return out
				}
			}
		}
	}

	var wg sync.WaitGroup
	var got1, got2 []byte
	wg.Add(2)
	go func() { defer wg.Done(); got1 = collect(sub1) }()
	go func() { defer wg.Done(); got2 = collect(sub2) }()
	wg.Wait()

	if !bytes.Contains(got1, []byte("AAA")) || !bytes.Contains(got1, []byte("BBB")) {
		t.Fatalf("client 1 did not receive pre+post failover chunks: len=%d", len(got1))
	}
	if !bytes.Contains(got2, []byte("BBB")) {
		t.Fatalf("client 2 did not receive failover chunks: len=%d", len(got2))
	}

	scans := dbStore.GetAllScans()
	if rec, ok := scans[id1]; !ok || rec.ConsecutiveFails == 0 {
		t.Fatalf("expected failed upstream %s to be penalized in scans DB, got %+v", id1, rec)
	}

	mu.Lock()
	stopped := stopCalls
	mu.Unlock()
	if stopped == 0 {
		t.Fatalf("expected StopEngineStream to invoke ?method=stop on failover")
	}
}

func TestHubDirectEngineJSONSessionAndStop(t *testing.T) {
	id1 := "3333333333333333333333333333333333333333"
	sessionHash := "abc123session"

	var stopCalled bool
	var mu sync.Mutex
	var engineSrv *httptest.Server
	engineSrv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ace/getstream":
			if r.URL.Query().Get("format") == "json" && r.URL.Query().Get("id") == id1 {
				// Intentionally return a container-local host (127.0.0.1:6878) to verify URL rewriting to engineURL.
				_, _ = w.Write([]byte(`{"response":{"playback_url":"http://127.0.0.1:6878/ace/r/` + id1 + `/` + sessionHash + `","command_url":"http://127.0.0.1:6878/ace/cmd/` + id1 + `/` + sessionHash + `","stat_url":"http://127.0.0.1:6878/ace/stat/` + id1 + `/` + sessionHash + `"}}`))
				return
			}
			w.WriteHeader(http.StatusNotFound)
		case "/ace/r/" + id1 + "/" + sessionHash:
			flusher, _ := w.(http.Flusher)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(append([]byte{0x47}, bytes.Repeat([]byte{'D'}, 187)...))
			if flusher != nil {
				flusher.Flush()
			}
			time.Sleep(250 * time.Millisecond)
		case "/ace/stat/" + id1 + "/" + sessionHash:
			_, _ = w.Write([]byte(`{"response":{"peers":27}}`))
		case "/ace/cmd/" + id1 + "/" + sessionHash:
			if r.URL.Query().Get("method") == "stop" {
				mu.Lock()
				stopCalled = true
				mu.Unlock()
			}
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer engineSrv.Close()

	t.Setenv("ACESTREAM_URL", "")
	t.Setenv("ENGINE_URL", engineSrv.URL)

	dir := t.TempDir()
	cfgMgr := config.NewManager(dir)
	dbStore, err := db.Open(dir)
	if err != nil {
		t.Fatalf("db open failed: %v", err)
	}
	defer dbStore.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	chanStore := channels.NewStore(cfgMgr, dbStore)
	hub := NewHub(ctx, cfgMgr, dbStore, chanStore)

	if hub.ProxyURL() != engineSrv.URL {
		t.Fatalf("expected direct engine ProxyURL=%s when ACESTREAM_URL is unset, got %s", engineSrv.URL, hub.ProxyURL())
	}

	ch := channels.Channel{
		GuideNumber: "1002",
		GuideName:   "Direct Engine TV",
		AceIDs:      []string{id1},
	}

	sub, err := hub.Subscribe(ch, "c1", "10.0.0.1", "VLC")
	if err != nil {
		t.Fatalf("Subscribe direct engine failed: %v", err)
	}

	var got []byte
	deadline := time.After(2 * time.Second)
Loop:
	for {
		select {
		case <-deadline:
			break Loop
		case chunk, open := <-sub.Ch:
			if !open {
				break Loop
			}
			got = append(got, chunk...)
			if bytes.Contains(got, []byte("DDD")) {
				break Loop
			}
		}
	}
	if !bytes.Contains(got, []byte("DDD")) {
		t.Fatalf("did not receive video bytes from playback_url: len=%d", len(got))
	}

	if peers := hub.FetchPeers(id1); peers != 27 {
		t.Fatalf("expected FetchPeers to read 27 from session stat_url, got %d", peers)
	}

	sub.Release()

	// Wait briefly for goroutine executing StopEngineStream to reach engineSrv.
	waitDeadline := time.Now().Add(1 * time.Second)
	for time.Now().Before(waitDeadline) {
		mu.Lock()
		stopped := stopCalled
		mu.Unlock()
		if stopped {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("expected releasing last client to invoke command_url?method=stop")
}
