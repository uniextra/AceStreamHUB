package scanner

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net/http"
	"os/exec"
	"time"

	"acestreamhub/internal/channels"
	"acestreamhub/internal/config"
	"acestreamhub/internal/db"
	"acestreamhub/internal/stream"
)

// ProbeResult summarizes the test result for a single AceStream ID.
type ProbeResult struct {
	ID      string  `json:"id"`
	Success bool    `json:"success"`
	Time    float64 `json:"time"`
	Info    string  `json:"info"`
}

// ScanSummary holds the full outcome of scanning a channel's AceStream sources.
type ScanSummary struct {
	Success bool          `json:"success"`
	BestID  string        `json:"best_id,omitempty"`
	Results []ProbeResult `json:"results,omitempty"`
	Error   string        `json:"error,omitempty"`
}

// Scanner probes candidate AceStream IDs with engine stats + ffprobe and scores them.
type Scanner struct {
	cfgMgr    *config.Manager
	dbStore   *db.Store
	chanStore *channels.Store
	hub       *stream.Hub
}

// NewScanner constructs a Scanner instance.
func NewScanner(cfgMgr *config.Manager, dbStore *db.Store, chanStore *channels.Store, hub *stream.Hub) *Scanner {
	return &Scanner{
		cfgMgr:    cfgMgr,
		dbStore:   dbStore,
		chanStore: chanStore,
		hub:       hub,
	}
}

// PerformChannelScan tests each Ace ID on channel chno (optionally capped at limit) and updates preferred_id.
func (s *Scanner) PerformChannelScan(ctx context.Context, chno string, limit int) ScanSummary {
	if s.hub.HasOtherActiveChannel(chno) {
		log.Printf("Skipping channel scan for %s: active playback is in progress", chno)
		return ScanSummary{Error: "Skipped due to active playback"}
	}

	channel, ok := s.chanStore.FindChannel(chno)
	if !ok {
		return ScanSummary{Error: "Channel not found"}
	}

	aceIDs := channel.AceIDs
	if len(aceIDs) == 0 {
		return ScanSummary{Error: "No IDs found"}
	}
	if limit > 0 && limit < len(aceIDs) {
		aceIDs = aceIDs[:limit]
	}

	var bestID string
	bestScore := math.Inf(-1)
	results := make([]ProbeResult, 0, len(aceIDs))
	httpClient := &http.Client{Timeout: 10 * time.Second}

	for _, aid := range aceIDs {
		start := time.Now()
		success := false
		info := ""
		resLabel := "SD"
		peers := 0
		speedKbps := 0.0
		var commandURL string

		func() {
			defer func() {
				if commandURL != "" {
					s.hub.StopEngineStream(aid, commandURL)
				}
			}()

			getStreamURL := fmt.Sprintf("%s/ace/getstream?id=%s&format=json", s.hub.EngineURL(), aid)
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, getStreamURL, nil)
			if err != nil {
				log.Printf("Test error for %s: %v", aid, err)
				return
			}
			resp, err := httpClient.Do(req)
			if err != nil {
				log.Printf("Test error for %s: %v", aid, err)
				return
			}
			var gsPayload struct {
				Response *struct {
					StatURL     string `json:"stat_url"`
					PlaybackURL string `json:"playback_url"`
					CommandURL  string `json:"command_url"`
				} `json:"response"`
			}
			decErr := json.NewDecoder(resp.Body).Decode(&gsPayload)
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusOK || decErr != nil || gsPayload.Response == nil {
				return
			}

			statURL := gsPayload.Response.StatURL
			playbackURL := gsPayload.Response.PlaybackURL
			commandURL = gsPayload.Response.CommandURL
			if statURL == "" || playbackURL == "" {
				return
			}

			select {
			case <-ctx.Done():
				return
			case <-time.After(2500 * time.Millisecond):
			}

			statReq, err := http.NewRequestWithContext(ctx, http.MethodGet, statURL, nil)
			if err == nil {
				statClient := &http.Client{Timeout: 5 * time.Second}
				if statResp, err := statClient.Do(statReq); err == nil {
					if statResp.StatusCode == http.StatusOK {
						var statPayload struct {
							Response *struct {
								Peers     int     `json:"peers"`
								SpeedDown float64 `json:"speed_down"`
							} `json:"response"`
						}
						if json.NewDecoder(statResp.Body).Decode(&statPayload) == nil && statPayload.Response != nil {
							peers = statPayload.Response.Peers
							speedKbps = statPayload.Response.SpeedDown
						}
					}
					_ = statResp.Body.Close()
				}
			}

			brMbps := (speedKbps * 8.0) / 1000.0
			brLabel := fmt.Sprintf("%.1f Mbps", brMbps)
			probeURL := fmt.Sprintf("%s/ace/getstream?id=%s", s.hub.ProxyURL(), aid)

			probeCtx, probeCancel := context.WithTimeout(ctx, 10*time.Second)
			cmd := exec.CommandContext(probeCtx, "ffprobe", "-v", "quiet", "-print_format", "json", "-show_streams", probeURL)
			stdout, probeErr := cmd.Output()
			probeCancel()
			if probeErr == nil {
				var probeData struct {
					Streams []struct {
						CodecType string `json:"codec_type"`
						Height    int    `json:"height"`
					} `json:"streams"`
				}
				if json.Unmarshal(stdout, &probeData) == nil {
					for _, st := range probeData.Streams {
						if st.CodecType == "video" {
							success = true
							if st.Height >= 1080 {
								resLabel = "FHD"
							} else if st.Height >= 720 {
								resLabel = "HD"
							}
							break
						}
					}
				}
			}

			if !success && peers > 0 {
				success = true
			}
			info = fmt.Sprintf("%s | %s | Peers: %d", resLabel, brLabel, peers)
		}()

		elapsed := time.Since(start).Seconds()
		score := math.Inf(-1)
		if success {
			score = 0
			switch resLabel {
			case "FHD":
				score += 1000
			case "HD":
				score += 500
			}
			score += float64(peers)
			score -= elapsed * 5
		}

		results = append(results, ProbeResult{
			ID:      aid,
			Success: success,
			Time:    elapsed,
			Info:    info,
		})
		if success && score > bestScore {
			bestScore = score
			bestID = aid
		}
	}

	if bestID != "" {
		nowStr := time.Now().Format("2006-01-02 15:04:05")
		_, _ = s.cfgMgr.Update(func(c *config.Config) {
			c.PreferredIDs[chno] = bestID
			c.LastScans[chno] = nowStr
		})
		for _, res := range results {
			s.dbStore.SaveScan(res.ID, res.Success, res.Time, res.Info)
		}
		s.chanStore.Rebuild()
		return ScanSummary{Success: true, BestID: bestID, Results: results}
	}

	return ScanSummary{Success: false, Results: results, Error: "No working sources found"}
}
