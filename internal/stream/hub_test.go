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
	t.Setenv("ENGINE_URL", proxySrv.URL)

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
