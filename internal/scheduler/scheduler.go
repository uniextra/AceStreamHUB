package scheduler

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"acestreamhub/internal/channels"
	"acestreamhub/internal/config"
	"acestreamhub/internal/scraper"
)

// Scheduler manages periodic background refreshes for all configured sources.
type Scheduler struct {
	mu        sync.Mutex
	cfgMgr    *config.Manager
	chanStore *channels.Store
	cancels   map[string]context.CancelFunc
	parentCtx context.Context
}

type rawCacheFile struct {
	Raw []scraper.RawChannel `json:"raw"`
}

type epgCacheFile struct {
	Channels   []scraper.EPGChannel   `json:"channels"`
	Programmes []scraper.EPGProgramme `json:"programmes"`
}

// NewScheduler constructs a Scheduler bound to parentCtx.
func NewScheduler(parentCtx context.Context, cfgMgr *config.Manager, chanStore *channels.Store) *Scheduler {
	return &Scheduler{
		cfgMgr:    cfgMgr,
		chanStore: chanStore,
		cancels:   make(map[string]context.CancelFunc),
		parentCtx: parentCtx,
	}
}

// Init loads all sources on startup (preferring disk cache) and launches refresh tickers.
func (s *Scheduler) Init() {
	s.mu.Lock()
	for id, cancel := range s.cancels {
		cancel()
		delete(s.cancels, id)
	}
	cfg := s.cfgMgr.Load()
	s.mu.Unlock()

	for _, src := range cfg.Sources {
		s.FetchSource(src.ID, false)

		interval := src.IntervalMinutes
		if interval <= 0 {
			interval = 60
		}
		ctx, cancel := context.WithCancel(s.parentCtx)
		s.mu.Lock()
		s.cancels[src.ID] = cancel
		s.mu.Unlock()

		go s.runTicker(ctx, src.ID, time.Duration(interval)*time.Minute)
	}
}

func (s *Scheduler) runTicker(ctx context.Context, sourceID string, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.FetchSource(sourceID, true)
		}
	}
}

// FetchSource updates a single source from network (or cache on boot when force==false).
func (s *Scheduler) FetchSource(sourceID string, force bool) {
	cfg := s.cfgMgr.Load()
	var target *config.Source
	for i := range cfg.Sources {
		if cfg.Sources[i].ID == sourceID {
			cp := cfg.Sources[i]
			target = &cp
			break
		}
	}
	if target == nil {
		return
	}

	cacheFile := filepath.Join(s.cfgMgr.DataDir(), fmt.Sprintf("cache_%s.json", sourceID))
	if !force {
		if data, err := os.ReadFile(cacheFile); err == nil {
			if target.Type == "epg" {
				var cached epgCacheFile
				if json.Unmarshal(data, &cached) == nil && len(cached.Channels) > 0 {
					log.Printf("Loaded EPG source %s from disk cache.", target.Name)
					s.chanStore.SetEPGSource(sourceID, cached.Channels, cached.Programmes)
					return
				}
			} else {
				var cached rawCacheFile
				if json.Unmarshal(data, &cached) == nil && len(cached.Raw) > 0 {
					log.Printf("Loaded source %s from disk cache.", target.Name)
					s.chanStore.SetRawSource(sourceID, cached.Raw)
					return
				}
			}
		}
	}

	var (
		raw        []scraper.RawChannel
		epgCh      []scraper.EPGChannel
		epgProg    []scraper.EPGProgramme
		fetchErr   error
		countLabel int
	)

	switch target.Type {
	case "m3u":
		raw, fetchErr = scraper.ParseM3U(target.URL)
		countLabel = len(raw)
	case "scrape":
		raw, fetchErr = scraper.ScrapeURL(target.URL)
		countLabel = len(raw)
	case "json":
		raw, fetchErr = scraper.ParseJSON(target.URL)
		countLabel = len(raw)
	case "epg":
		epgCh, epgProg, fetchErr = scraper.ParseEPG(target.URL)
		countLabel = len(epgCh)
	}

	if fetchErr == nil {
		if target.Type == "epg" {
			if payload, err := json.Marshal(epgCacheFile{Channels: epgCh, Programmes: epgProg}); err == nil {
				_ = os.WriteFile(cacheFile, payload, 0o644)
			}
		} else {
			if payload, err := json.Marshal(rawCacheFile{Raw: raw}); err == nil {
				_ = os.WriteFile(cacheFile, payload, 0o644)
			}
		}
	}

	nowStr := time.Now().Format("2006-01-02 15:04:05")
	statusStr := "Error"
	if fetchErr == nil {
		statusStr = fmt.Sprintf("OK (%d channels)", countLabel)
	}
	_, _ = s.cfgMgr.Update(func(c *config.Config) {
		for i := range c.Sources {
			if c.Sources[i].ID == sourceID {
				c.Sources[i].LastRun = &nowStr
				c.Sources[i].Status = statusStr
				break
			}
		}
	})

	if target.Type == "epg" {
		totalCh, totalProg := s.chanStore.SetEPGSource(sourceID, epgCh, epgProg)
		log.Printf("Merged %d total EPG channels and %d total programmes", totalCh, totalProg)
	} else {
		s.chanStore.SetRawSource(sourceID, raw)
	}
}
