package stream

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"acestreamhub/internal/channels"
	"acestreamhub/internal/config"
	"acestreamhub/internal/db"
)

const (
	chunkSize         = 64 * 1024
	clientQueueChunks = 64              // ~4 MB per client in volatile RAM
	ringMaxBytes      = 4 * 1024 * 1024 // ~4 MB RAM burst ring for instant playback start
)

// ClientMeta describes a connected viewer returned by /api/stats.
type ClientMeta struct {
	ID               string `json:"id"`
	IP               string `json:"ip"`
	UserAgent        string `json:"user_agent"`
	ConnectedSeconds int64  `json:"connected_seconds"`
	ConnectedSince   time.Time
}

// ChannelStats matches the JSON telemetry expected by dashboard.html.
type ChannelStats struct {
	ChannelID     string       `json:"channel_id"`
	ChannelName   string       `json:"channel_name"`
	AceID         string       `json:"ace_id"`
	Peers         int          `json:"peers"`
	Status        string       `json:"status"`
	Kbps          int64        `json:"kbps"`
	TotalMB       float64      `json:"total_mb"`
	ClientsCount  int          `json:"clients_count"`
	Clients       []ClientMeta `json:"clients"`
	UptimeSeconds int64        `json:"uptime_seconds"`
}

// StatsResponse matches /api/stats response shape.
type StatsResponse struct {
	ActiveClients       int            `json:"active_clients"`
	TotalActiveChannels int            `json:"total_active_channels"`
	Channels            []ChannelStats `json:"channels"`
}

// ClientSubscription represents one attached HTTP player consuming a multiplexed stream.
type ClientSubscription struct {
	ID string
	Ch <-chan []byte
	cs *ChannelStream
	h  *Hub
}

// Release detaches this client from the multiplexed ChannelStream.
func (sub *ClientSubscription) Release() {
	sub.h.detachClient(sub.cs, sub.ID)
}

type clientSlot struct {
	meta ClientMeta
	ch   chan []byte
}

type ChannelStream struct {
	channelID   string
	channelName string
	startedAt   time.Time

	mu         sync.RWMutex
	aceID      string
	status     string
	clients    map[string]*clientSlot
	ring       [][]byte
	ringBytes  int
	closed     bool
	cancelFunc context.CancelFunc
	activeBody io.ReadCloser

	totalBytes atomic.Int64
	kbps       atomic.Int64
}

type peersEntry struct {
	ts    time.Time
	peers int
}

// Hub multiplexes AceStream upstreams to multiple clients with volatile RAM buffering,
// MPEG-TS aligned Hot Failover, and on-demand engine session lifecycle management.
type Hub struct {
	cfgMgr    *config.Manager
	dbStore   *db.Store
	chanStore *channels.Store

	proxyURL  string
	engineURL string

	mu                   sync.RWMutex
	activeStreams        map[string]*ChannelStream
	lastClientDisconnect time.Time

	sessionsMu     sync.Mutex
	engineSessions map[string]string // aceID -> commandURL

	peersMu    sync.Mutex
	peersCache map[string]peersEntry

	streamHTTP *http.Client
	ctrlHTTP   *http.Client
}

// NewHub constructs a Hub and starts the background engine lifecycle worker.
func NewHub(ctx context.Context, cfgMgr *config.Manager, dbStore *db.Store, chanStore *channels.Store) *Hub {
	proxyURL := strings.TrimRight(os.Getenv("ACESTREAM_URL"), "/")
	if proxyURL == "" {
		proxyURL = "http://acexy:8080"
	}
	engineURL := strings.TrimRight(os.Getenv("ENGINE_URL"), "/")
	if engineURL == "" {
		engineURL = "http://acestream-engine:6878"
	}

	// Streaming HTTP client deliberately omits overall Timeout so live broadcasts stay open indefinitely;
	// initial connection timeouts are enforced via per-request contexts.
	transport := &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		MaxIdleConns:        32,
		IdleConnTimeout:     60 * time.Second,
		DisableCompression:  true,
	}

	h := &Hub{
		cfgMgr:               cfgMgr,
		dbStore:              dbStore,
		chanStore:            chanStore,
		proxyURL:             proxyURL,
		engineURL:            engineURL,
		activeStreams:        make(map[string]*ChannelStream),
		lastClientDisconnect: time.Now(),
		engineSessions:       make(map[string]string),
		peersCache:           make(map[string]peersEntry),
		streamHTTP:           &http.Client{Transport: transport},
		ctrlHTTP:             &http.Client{Timeout: 3 * time.Second},
	}

	go h.lifecycleWorker(ctx)
	return h
}

// ProxyURL returns the configured upstream proxy URL (acexy or engine).
func (h *Hub) ProxyURL() string { return h.proxyURL }

// EngineURL returns the configured raw AceStream engine API URL.
func (h *Hub) EngineURL() string { return h.engineURL }

// RegisterEngineSession tracks an active engine command_url for later teardown.
func (h *Hub) RegisterEngineSession(aceID, commandURL string) {
	if aceID == "" || commandURL == "" {
		return
	}
	h.sessionsMu.Lock()
	h.engineSessions[aceID] = commandURL
	h.sessionsMu.Unlock()
}

// StopEngineStream sends an explicit method=stop to AceStream Engine for aceID/commandURL.
func (h *Hub) StopEngineStream(aceID, commandURL string) {
	if aceID == "" && commandURL == "" {
		return
	}

	var urls []string
	if commandURL != "" {
		urls = append(urls, appendStopMethod(commandURL))
	}
	h.sessionsMu.Lock()
	if cached, ok := h.engineSessions[aceID]; ok {
		delete(h.engineSessions, aceID)
		if cached != "" && cached != commandURL {
			urls = append(urls, appendStopMethod(cached))
		}
	}
	h.sessionsMu.Unlock()

	for _, u := range urls {
		resp, err := h.ctrlHTTP.Get(u)
		if err != nil {
			continue
		}
		_ = resp.Body.Close()
		log.Printf("Engine stream explicitly stopped via: %s", u)
	}
}

func appendStopMethod(rawURL string) string {
	if strings.Contains(rawURL, "?") {
		return rawURL + "&method=stop"
	}
	return rawURL + "?method=stop"
}

// CleanupAll immediately stops all tracked AceStream engine sessions and returns count stopped.
func (h *Hub) CleanupAll() int {
	h.sessionsMu.Lock()
	snapshot := make(map[string]string, len(h.engineSessions))
	for k, v := range h.engineSessions {
		snapshot[k] = v
	}
	h.sessionsMu.Unlock()

	for aid, cmdURL := range snapshot {
		h.StopEngineStream(aid, cmdURL)
	}
	return len(snapshot)
}

func (h *Hub) lifecycleWorker(ctx context.Context) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			cfg := h.cfgMgr.Load()
			if !cfg.EngineIdleCleanupEnabled() {
				continue
			}
			idleTimeout := time.Duration(cfg.EngineIdleTimeout()) * time.Second

			h.mu.RLock()
			clientsCount := h.totalClientsUnlocked()
			idleDuration := time.Since(h.lastClientDisconnect)
			h.mu.RUnlock()

			if clientsCount == 0 && idleDuration >= idleTimeout {
				h.sessionsMu.Lock()
				hasSessions := len(h.engineSessions) > 0
				h.sessionsMu.Unlock()
				if hasSessions {
					reclaimed := h.CleanupAll()
					if reclaimed > 0 {
						log.Printf("Engine Lifecycle: System idle for %ds; reclaimed %d session(s).", int(idleDuration.Seconds()), reclaimed)
					}
				}
			}
		}
	}
}

func (h *Hub) totalClientsUnlocked() int {
	total := 0
	for _, cs := range h.activeStreams {
		cs.mu.RLock()
		total += len(cs.clients)
		cs.mu.RUnlock()
	}
	return total
}

// ActiveClientCount returns the total number of viewers across all active channels.
func (h *Hub) ActiveClientCount() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.totalClientsUnlocked()
}

// ActiveChannelIDs returns a slice of currently streaming channel IDs.
func (h *Hub) ActiveChannelIDs() []string {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make([]string, 0, len(h.activeStreams))
	for id := range h.activeStreams {
		out = append(out, id)
	}
	return out
}

// HasOtherActiveChannel reports whether another channel besides chno currently has viewers.
func (h *Hub) HasOtherActiveChannel(chno string) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for id := range h.activeStreams {
		if id != chno {
			return true
		}
	}
	return false
}

var ErrAllSourcesFailed = errors.New("channel stream unavailable: all sources failed")

// Subscribe attaches clientID to channel's live multiplexed stream, starting an upstream
// connection if the channel is not currently active.
func (h *Hub) Subscribe(channel channels.Channel, clientID, clientIP, userAgent string) (*ClientSubscription, error) {
	cfg := h.cfgMgr.Load()
	autoDiscard := cfg.AutoDiscardEnabled()
	hotFailover := cfg.HotFailoverEnabled()
	timeoutSec := cfg.StreamTimeout()
	if timeoutSec <= 0 {
		timeoutSec = 30
	}

	// Fast path: join already-active multiplexed stream for this channel.
	h.mu.RLock()
	existing := h.activeStreams[channel.GuideNumber]
	h.mu.RUnlock()
	if existing != nil {
		if sub := h.attachToStream(existing, clientID, clientIP, userAgent); sub != nil {
			return sub, nil
		}
	}

	validAceIDs := append([]string(nil), channel.AceIDs...)
	if autoDiscard && h.dbStore != nil {
		scans := h.dbStore.GetAllScans()
		var filtered []string
		for _, aid := range validAceIDs {
			if rec, ok := scans[aid]; !ok || rec.ConsecutiveFails < 3 {
				filtered = append(filtered, aid)
			}
		}
		if len(filtered) > 0 {
			validAceIDs = filtered
		}
	}

	resp, connectedID, err := h.connectFirstWorking(validAceIDs, channel.GuideNumber, channel.GuideName, time.Duration(timeoutSec)*time.Second)
	if err != nil {
		return nil, err
	}

	streamCtx, cancel := context.WithCancel(context.Background())

	h.mu.Lock()
	// Re-check in case another goroutine started the channel while we were dialing.
	if race := h.activeStreams[channel.GuideNumber]; race != nil {
		h.mu.Unlock()
		_ = resp.Body.Close()
		cancel()
		if sub := h.attachToStream(race, clientID, clientIP, userAgent); sub != nil {
			return sub, nil
		}
		return nil, ErrAllSourcesFailed
	}

	cs := &ChannelStream{
		channelID:   channel.GuideNumber,
		channelName: channel.GuideName,
		startedAt:   time.Now(),
		aceID:       connectedID,
		status:      "DIRECTO (Pass-Through)",
		clients:     make(map[string]*clientSlot),
		cancelFunc:  cancel,
	}
	sub := cs.addClient(clientID, clientIP, userAgent, h)
	h.activeStreams[channel.GuideNumber] = cs
	h.mu.Unlock()

	go h.runProducer(streamCtx, cs, resp, connectedID, validAceIDs, channel.AceIDs, hotFailover, timeoutSec)
	return sub, nil
}

func (h *Hub) attachToStream(cs *ChannelStream, clientID, clientIP, userAgent string) *ClientSubscription {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	if cs.closed {
		return nil
	}
	sub := cs.addClientLocked(clientID, clientIP, userAgent, h)
	slotCh := cs.clients[clientID].ch
	// Replay buffered RAM ring chunks immediately so late joiners start smoothly.
	for _, pkt := range cs.ring {
		select {
		case slotCh <- pkt:
		default:
		}
	}
	return sub
}

func (cs *ChannelStream) addClient(clientID, clientIP, userAgent string, h *Hub) *ClientSubscription {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	return cs.addClientLocked(clientID, clientIP, userAgent, h)
}

func (cs *ChannelStream) addClientLocked(clientID, clientIP, userAgent string, h *Hub) *ClientSubscription {
	ch := make(chan []byte, clientQueueChunks)
	cs.clients[clientID] = &clientSlot{
		meta: ClientMeta{
			ID:             clientID,
			IP:             clientIP,
			UserAgent:      userAgent,
			ConnectedSince: time.Now(),
		},
		ch: ch,
	}
	return &ClientSubscription{
		ID: clientID,
		Ch: ch,
		cs: cs,
		h:  h,
	}
}

func (h *Hub) detachClient(cs *ChannelStream, clientID string) {
	if cs == nil {
		return
	}
	var shouldStop bool

	h.mu.Lock()
	cs.mu.Lock()
	if slot, ok := cs.clients[clientID]; ok {
		close(slot.ch)
		delete(cs.clients, clientID)
	}
	if len(cs.clients) == 0 && !cs.closed {
		cs.closed = true
		shouldStop = true
		if cur, ok := h.activeStreams[cs.channelID]; ok && cur == cs {
			delete(h.activeStreams, cs.channelID)
		}
		if h.totalClientsUnlocked() == 0 {
			h.lastClientDisconnect = time.Now()
		}
	}
	aceID := cs.aceID
	cancel := cs.cancelFunc
	cs.mu.Unlock()
	h.mu.Unlock()

	if shouldStop {
		if cancel != nil {
			cancel()
		}
		cs.closeActiveBody()
		go h.StopEngineStream(aceID, "")
	}
}

func (cs *ChannelStream) setActiveBody(body io.ReadCloser) {
	cs.mu.Lock()
	cs.activeBody = body
	cs.mu.Unlock()
}

func (cs *ChannelStream) closeActiveBody() {
	cs.mu.Lock()
	body := cs.activeBody
	cs.activeBody = nil
	cs.mu.Unlock()
	if body != nil {
		_ = body.Close()
	}
}

func (h *Hub) connectFirstWorking(ids []string, chID, chName string, timeout time.Duration) (*http.Response, string, error) {
	for _, aceID := range ids {
		streamURL := fmt.Sprintf("%s/ace/getstream?id=%s", h.proxyURL, aceID)
		log.Printf("Connecting channel %s (%s) from %s (timeout=%v)", chID, chName, streamURL, timeout)
		resp, err := h.dialLiveStream(streamURL, timeout)
		if err != nil {
			log.Printf("Fallback: failed to connect to %s: %v", streamURL, err)
			continue
		}
		return resp, aceID, nil
	}
	return nil, "", ErrAllSourcesFailed
}

// dialLiveStream opens an upstream stream using ResponseHeaderTimeout so the TCP connection
// remains open indefinitely for live broadcasts once HTTP 200 headers arrive.
func (h *Hub) dialLiveStream(streamURL string, headerTimeout time.Duration) (*http.Response, error) {
	client := &http.Client{
		Transport: &http.Transport{
			DialContext: (&net.Dialer{
				Timeout:   headerTimeout,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			ResponseHeaderTimeout: headerTimeout,
			DisableCompression:    true,
		},
	}
	resp, err := client.Get(streamURL)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return resp, nil
}

func (h *Hub) runProducer(
	ctx context.Context,
	cs *ChannelStream,
	initialResp *http.Response,
	initialID string,
	validAceIDs []string,
	allChannelIDs []string,
	hotFailover bool,
	timeoutSec int,
) {
	currentResp := initialResp
	currentID := initialID
	cs.setActiveBody(currentResp.Body)
	failedIDs := make(map[string]struct{})
	buf := make([]byte, chunkSize)

	var speedBytes int64
	speedStart := time.Now()
	alignNext := false

	defer func() {
		cs.closeActiveBody()
		h.StopEngineStream(currentID, "")
		h.closeStream(cs)
	}()

	for {
		if ctx.Err() != nil {
			return
		}

		n, err := currentResp.Body.Read(buf)
		if n > 0 {
			chunk := make([]byte, n)
			copy(chunk, buf[:n])
			if alignNext {
				chunk = alignMPEGTS(chunk)
				alignNext = false
			}
			if len(chunk) > 0 {
				cs.totalBytes.Add(int64(len(chunk)))
				speedBytes += int64(len(chunk))
				if elapsed := time.Since(speedStart).Seconds(); elapsed >= 1.0 {
					cs.kbps.Store(int64(float64(speedBytes*8) / (1024.0 * elapsed)))
					speedBytes = 0
					speedStart = time.Now()
				}
				cs.broadcast(chunk)
			}
		}

		if err != nil {
			if ctx.Err() != nil {
				return
			}
			if err != io.EOF {
				log.Printf("Upstream stream error on %s for channel %s: %v", currentID, cs.channelID, err)
			} else {
				log.Printf("Upstream stream EOF on %s for channel %s", currentID, cs.channelID)
			}

			if !hotFailover {
				return
			}

			failedIDs[currentID] = struct{}{}
			log.Printf("[HOT FAILOVER] Upstream dropped on %s for channel %s. Seeking failover...", currentID, cs.channelID)
			if h.dbStore != nil {
				h.dbStore.SaveScan(currentID, false, 0.0, "Desconexión en directo (Hot Failover)")
			}

			_ = currentResp.Body.Close()
			h.StopEngineStream(currentID, "")

			candidates := buildCandidates(validAceIDs, allChannelIDs, failedIDs, currentID)
			if len(candidates) == 0 {
				log.Printf("[HOT FAILOVER] No alternative Ace IDs available for channel %s.", cs.channelID)
				return
			}

			cs.setStatus("Recuperando (Hot Failover)...", currentID)

			failoverTimeout := timeoutSec
			if failoverTimeout > 15 {
				failoverTimeout = 15
			}
			var nextResp *http.Response
			var nextID string
			for _, candidate := range candidates {
				if ctx.Err() != nil {
					return
				}
				log.Printf("[HOT FAILOVER] Trying failover to %s for channel %s...", candidate, cs.channelID)
				nextURL := fmt.Sprintf("%s/ace/getstream?id=%s", h.proxyURL, candidate)
				r, dialErr := h.dialLiveStream(nextURL, time.Duration(failoverTimeout)*time.Second)
				if dialErr == nil {
					nextResp = r
					nextID = candidate
					break
				}
				log.Printf("[HOT FAILOVER] Candidate %s failed: %v", candidate, dialErr)
				failedIDs[candidate] = struct{}{}
			}

			if nextResp == nil {
				log.Printf("[HOT FAILOVER] All failover candidates failed for channel %s.", cs.channelID)
				return
			}

			currentResp = nextResp
			currentID = nextID
			cs.setActiveBody(currentResp.Body)
			alignNext = true
			cs.setStatus("DIRECTO (Failover Activo)", nextID)
			log.Printf("[HOT FAILOVER] SUCCESS: Channel %s recovered seamlessly with ID %s", cs.channelID, nextID)
		}
	}
}

func alignMPEGTS(data []byte) []byte {
	idx := bytes.IndexByte(data, 0x47)
	if idx > 0 && idx < 188 {
		return data[idx:]
	}
	return data
}

func buildCandidates(validIDs, allIDs []string, failed map[string]struct{}, currentID string) []string {
	var out []string
	seen := make(map[string]struct{})
	for _, id := range validIDs {
		if _, bad := failed[id]; !bad && id != currentID {
			if _, dup := seen[id]; !dup {
				seen[id] = struct{}{}
				out = append(out, id)
			}
		}
	}
	if len(out) == 0 {
		for _, id := range allIDs {
			if _, bad := failed[id]; !bad && id != currentID {
				if _, dup := seen[id]; !dup {
					seen[id] = struct{}{}
					out = append(out, id)
				}
			}
		}
	}
	return out
}

func (cs *ChannelStream) setStatus(status, aceID string) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	cs.status = status
	if aceID != "" {
		cs.aceID = aceID
	}
}

func (cs *ChannelStream) broadcast(chunk []byte) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	if cs.closed {
		return
	}

	// Update volatile RAM ring buffer.
	cs.ring = append(cs.ring, chunk)
	cs.ringBytes += len(chunk)
	for cs.ringBytes > ringMaxBytes && len(cs.ring) > 1 {
		cs.ringBytes -= len(cs.ring[0])
		cs.ring = cs.ring[1:]
	}

	for _, slot := range cs.clients {
		select {
		case slot.ch <- chunk:
		default:
			// Evict oldest chunk on a lagging client so healthy viewers never stall.
			select {
			case <-slot.ch:
			default:
			}
			select {
			case slot.ch <- chunk:
			default:
			}
		}
	}
}

func (h *Hub) closeStream(cs *ChannelStream) {
	h.mu.Lock()
	cs.mu.Lock()
	if !cs.closed {
		cs.closed = true
		for id, slot := range cs.clients {
			close(slot.ch)
			delete(cs.clients, id)
		}
	}
	if cur, ok := h.activeStreams[cs.channelID]; ok && cur == cs {
		delete(h.activeStreams, cs.channelID)
	}
	if h.totalClientsUnlocked() == 0 {
		h.lastClientDisconnect = time.Now()
	}
	cs.mu.Unlock()
	h.mu.Unlock()
}

// FetchPeers queries the proxy/engine stat API with a 2.5-second RAM cache.
func (h *Hub) FetchPeers(aceID string) int {
	if aceID == "" {
		return 0
	}
	now := time.Now()
	h.peersMu.Lock()
	if cached, ok := h.peersCache[aceID]; ok && now.Sub(cached.ts) < 2500*time.Millisecond {
		h.peersMu.Unlock()
		return cached.peers
	}
	h.peersMu.Unlock()

	endpoints := []string{
		fmt.Sprintf("%s/ace/stat?id=%s", h.proxyURL, aceID),
		fmt.Sprintf("%s/ace/stat?id=%s", h.engineURL, aceID),
		fmt.Sprintf("%s/stat/%s", h.engineURL, aceID),
		fmt.Sprintf("%s/stat/%s", h.proxyURL, aceID),
	}

	statClient := &http.Client{Timeout: 1 * time.Second}
	peers := -1
	for _, endpoint := range endpoints {
		resp, err := statClient.Get(endpoint)
		if err != nil {
			continue
		}
		if resp.StatusCode == http.StatusOK {
			var parsed map[string]any
			if decErr := json.NewDecoder(resp.Body).Decode(&parsed); decErr == nil {
				if respMap, ok := parsed["response"].(map[string]any); ok {
					if p, ok := extractInt(respMap["peers"]); ok {
						peers = p
					}
				} else if p, ok := extractInt(parsed["peers"]); ok {
					peers = p
				}
			}
		}
		_ = resp.Body.Close()
		if peers >= 0 {
			break
		}
	}

	h.peersMu.Lock()
	defer h.peersMu.Unlock()
	if peers >= 0 {
		h.peersCache[aceID] = peersEntry{ts: now, peers: peers}
		return peers
	}
	old := 0
	if cached, ok := h.peersCache[aceID]; ok {
		old = cached.peers
	}
	h.peersCache[aceID] = peersEntry{ts: now, peers: old}
	return old
}

func extractInt(v any) (int, bool) {
	switch t := v.(type) {
	case float64:
		return int(t), true
	case int:
		return t, true
	case int64:
		return int(t), true
	}
	return 0, false
}

// GetStats collects real-time telemetry across all active channels and clients.
func (h *Hub) GetStats() StatsResponse {
	now := time.Now()

	h.mu.RLock()
	streamsSnapshot := make([]*ChannelStream, 0, len(h.activeStreams))
	for _, cs := range h.activeStreams {
		streamsSnapshot = append(streamsSnapshot, cs)
	}
	h.mu.RUnlock()

	out := StatsResponse{
		Channels: make([]ChannelStats, 0, len(streamsSnapshot)),
	}

	for _, cs := range streamsSnapshot {
		cs.mu.RLock()
		clients := make([]ClientMeta, 0, len(cs.clients))
		for _, slot := range cs.clients {
			meta := slot.meta
			meta.ConnectedSeconds = int64(now.Sub(meta.ConnectedSince).Seconds())
			clients = append(clients, meta)
		}
		aceID := cs.aceID
		status := cs.status
		cs.mu.RUnlock()

		out.ActiveClients += len(clients)
		peersCount := 0
		if aceID != "" {
			peersCount = h.FetchPeers(aceID)
		}

		totalMB := float64(cs.totalBytes.Load()) / (1024.0 * 1024.0)
		// Round to 1 decimal place like Python's round(..., 1)
		totalMB = float64(int(totalMB*10+0.5)) / 10.0

		out.Channels = append(out.Channels, ChannelStats{
			ChannelID:     cs.channelID,
			ChannelName:   cs.channelName,
			AceID:         aceID,
			Peers:         peersCount,
			Status:        status,
			Kbps:          cs.kbps.Load(),
			TotalMB:       totalMB,
			ClientsCount:  len(clients),
			Clients:       clients,
			UptimeSeconds: int64(now.Sub(cs.startedAt).Seconds()),
		})
	}
	out.TotalActiveChannels = len(out.Channels)
	return out
}
