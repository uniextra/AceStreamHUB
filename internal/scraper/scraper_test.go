package scraper

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestParseM3U(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, "#EXTM3U\n#EXTINF:-1,Channel One\nacestream://1111111111111111111111111111111111111111\n#EXTINF:-1,Channel Two\nhttp://127.0.0.1:6878/ace/getstream?id=2222222222222222222222222222222222222222\n")
	}))
	defer srv.Close()

	chs, err := ParseM3U(srv.URL)
	if err != nil {
		t.Fatalf("ParseM3U error: %v", err)
	}
	if len(chs) != 2 {
		t.Fatalf("expected 2 channels, got %d", len(chs))
	}
	if chs[0].Title != "Channel One" || chs[0].AceID != "1111111111111111111111111111111111111111" {
		t.Fatalf("unexpected channel 0: %+v", chs[0])
	}
	if chs[1].Title != "Channel Two" || chs[1].AceID != "2222222222222222222222222222222222222222" {
		t.Fatalf("unexpected channel 1: %+v", chs[1])
	}
}

func TestParseJSONAndScrapeURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/json" {
			_, _ = fmt.Fprint(w, `{"hashes":[{"title":"La 1","hash":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","tvg_id":"la1","logo":"http://img/1.png"}]}`)
			return
		}
		_, _ = fmt.Fprint(w, `<html><body><a href="acestream://bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb">Sport HD</a></body></html>`)
	}))
	defer srv.Close()

	jsonChs, err := ParseJSON(srv.URL + "/json")
	if err != nil || len(jsonChs) != 1 || jsonChs[0].ProvidedTVGID != "la1" {
		t.Fatalf("unexpected ParseJSON result: err=%v chs=%+v", err, jsonChs)
	}

	htmlChs, err := ScrapeURL(srv.URL + "/html")
	if err != nil || len(htmlChs) != 1 || htmlChs[0].Title != "Sport HD" {
		t.Fatalf("unexpected ScrapeURL result: err=%v chs=%+v", err, htmlChs)
	}
}

func TestParseEPG(t *testing.T) {
	now := time.Now().UTC()
	futureStart := now.Add(10 * time.Minute).Format("20060102150405 -0700")
	futureStop := now.Add(70 * time.Minute).Format("20060102150405 -0700")
	pastStart := now.Add(-120 * time.Minute).Format("20060102150405 -0700")
	pastStop := now.Add(-60 * time.Minute).Format("20060102150405 -0700")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?>
<tv>
  <channel id="la1.es"><display-name>La 1</display-name><icon src="http://img/la1.png"/></channel>
  <programme start="%s" stop="%s" channel="la1.es"><title>Future Show</title><desc>Live</desc></programme>
  <programme start="%s" stop="%s" channel="la1.es"><title>Old Show</title></programme>
</tv>`, futureStart, futureStop, pastStart, pastStop)
	}))
	defer srv.Close()

	chs, progs, err := ParseEPG(srv.URL)
	if err != nil {
		t.Fatalf("ParseEPG error: %v", err)
	}
	if len(chs) != 1 || chs[0].Logo != "http://img/la1.png" {
		t.Fatalf("unexpected EPG channels: %+v", chs)
	}
	if len(progs) != 1 || progs[0].Title != "Future Show" {
		t.Fatalf("expected only 1 future programme, got %+v", progs)
	}
}
