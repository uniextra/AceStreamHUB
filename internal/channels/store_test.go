package channels

import (
	"testing"

	"acestreamhub/internal/config"
	"acestreamhub/internal/db"
	"acestreamhub/internal/scraper"
)

func TestGetStableIDDeterministic(t *testing.T) {
	id1 := GetStableID("La 1")
	id2 := GetStableID("La 1")
	if id1 != "4249" {
		t.Fatalf("expected Python-compatible ID 4249 for La 1, got %s", id1)
	}
	if id1 != id2 {
		t.Fatalf("expected deterministic ID, got %s vs %s", id1, id2)
	}
	if len(id1) != 4 {
		t.Fatalf("expected 4-digit ID, got %q", id1)
	}
	if GetStableID("Antena 3") == id1 {
		t.Fatalf("unexpected collision for different names")
	}
}

func TestTokenSetRatio(t *testing.T) {
	if score := TokenSetRatio("la 1", "la 1 tve"); score != 100 {
		t.Fatalf("expected 100 for subset match, got %d", score)
	}
	if score := TokenSetRatio("movistar liga", "liga movistar"); score != 100 {
		t.Fatalf("expected 100 for reordered tokens, got %d", score)
	}
	if score := TokenSetRatio("telecinco", "bbc news"); score >= 85 {
		t.Fatalf("expected low score for unrelated names, got %d", score)
	}
}

func TestRebuildOrderingBlacklistAndEPG(t *testing.T) {
	dir := t.TempDir()
	cfgMgr := config.NewManager(dir)
	dbStore, err := db.Open(dir)
	if err != nil {
		t.Fatalf("db open failed: %v", err)
	}
	defer dbStore.Close()

	badID := "0000000000000000000000000000000000000001"
	goodID := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	prefID := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

	dbStore.SaveScan(goodID, true, 1.2, "FHD | 5.0 Mbps | Peers: 20")
	dbStore.SaveScan(prefID, true, 1.5, "HD | 3.0 Mbps | Peers: 5")

	chnoLa1 := GetStableID("La 1")
	_, err = cfgMgr.Update(func(c *config.Config) {
		c.BlacklistIDs = []string{badID}
		c.PreferredIDs[chnoLa1] = prefID
	})
	if err != nil {
		t.Fatalf("config update failed: %v", err)
	}

	store := NewStore(cfgMgr, dbStore)
	store.SetEPGSource("epg1", []scraper.EPGChannel{
		{ID: "la1.es", Name: "La 1", Logo: "http://logo/la1.png"},
	}, nil)
	store.SetRawSource("src1", []scraper.RawChannel{
		{Title: "La 1 FHD", AceID: goodID, Extinf: `#EXTINF:-1,La 1 FHD`},
		{Title: "La 1 --> Mirror", AceID: prefID},
		{Title: "La 1", AceID: badID},
		{Title: "Antena 3", AceID: "cccccccccccccccccccccccccccccccccccccccc"},
	})

	chs := store.Channels()
	if len(chs) != 2 {
		t.Fatalf("expected 2 unified channels, got %d", len(chs))
	}

	la1, ok := store.FindChannel(chnoLa1)
	if !ok {
		t.Fatalf("expected La 1 channel with GuideNumber %s", chnoLa1)
	}
	if len(la1.AceIDs) != 2 {
		t.Fatalf("expected 2 non-blacklisted Ace IDs, got %d: %v", len(la1.AceIDs), la1.AceIDs)
	}
	if la1.AceIDs[0] != prefID {
		t.Fatalf("expected preferred ID %s first, got %v", prefID, la1.AceIDs)
	}
	if la1.MappedEPGID != "la1.es" || la1.MappedEPGLogo != "http://logo/la1.png" {
		t.Fatalf("expected EPG auto-mapping to la1.es, got id=%q logo=%q", la1.MappedEPGID, la1.MappedEPGLogo)
	}
}
