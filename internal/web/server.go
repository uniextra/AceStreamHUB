package web

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"acestreamhub/internal/channels"
	"acestreamhub/internal/config"
	"acestreamhub/internal/db"
	"acestreamhub/internal/i18n"
	"acestreamhub/internal/scanner"
	"acestreamhub/internal/scheduler"
	"acestreamhub/internal/scraper"
	"acestreamhub/internal/stream"
)

// Server exposes the web dashboard, management REST APIs, M3U/EPG/HDHR feeds, and live streaming.
type Server struct {
	assetsFS  fs.FS
	tmpls     *templateSet
	cfgMgr    *config.Manager
	dbStore   *db.Store
	chanStore *channels.Store
	hub       *stream.Hub
	scanner   *scanner.Scanner
	sched     *scheduler.Scheduler
	mux       *http.ServeMux
}

// NewServer constructs a configured HTTP Server.
func NewServer(
	assetsFS fs.FS,
	cfgMgr *config.Manager,
	dbStore *db.Store,
	chanStore *channels.Store,
	hub *stream.Hub,
	scanSvc *scanner.Scanner,
	sched *scheduler.Scheduler,
) (*Server, error) {
	tmpls, err := loadTemplates(assetsFS)
	if err != nil {
		return nil, err
	}
	s := &Server{
		assetsFS:  assetsFS,
		tmpls:     tmpls,
		cfgMgr:    cfgMgr,
		dbStore:   dbStore,
		chanStore: chanStore,
		hub:       hub,
		scanner:   scanSvc,
		sched:     sched,
		mux:       http.NewServeMux(),
	}
	s.registerRoutes()
	return s, nil
}

// Handler returns the root http.Handler wrapped with security headers, CORS, and session auth.
func (s *Server) Handler() http.Handler {
	return s.middleware(s.mux)
}

func (s *Server) registerRoutes() {
	s.mux.Handle("GET /static/", http.FileServerFS(s.assetsFS))

	s.mux.HandleFunc("GET /login", s.handleLoginPage)
	s.mux.HandleFunc("POST /login", s.handleLoginPost)
	s.mux.HandleFunc("GET /logout", s.handleLogout)
	s.mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/dashboard", http.StatusFound)
	})

	s.mux.HandleFunc("GET /dashboard", s.handleDashboard)
	s.mux.HandleFunc("GET /sources", s.handleSourcesPage)
	s.mux.HandleFunc("GET /channels", s.handleChannelsPage)
	s.mux.HandleFunc("GET /epg", s.handleEPGPage)
	s.mux.HandleFunc("GET /settings", s.handleSettingsPage)

	s.mux.HandleFunc("GET /api/stats", s.handleAPIStats)
	s.mux.HandleFunc("POST /api/sources", s.handleAddSource)
	s.mux.HandleFunc("DELETE /api/sources/{source_id}", s.handleDeleteSource)
	s.mux.HandleFunc("POST /api/sources/{source_id}/refresh", s.handleRefreshSource)
	s.mux.HandleFunc("POST /api/settings", s.handleSaveSettings)
	s.mux.HandleFunc("POST /api/engine/cleanup", s.handleEngineCleanup)
	s.mux.HandleFunc("POST /api/reorder_channels", s.handleReorderChannels)
	s.mux.HandleFunc("POST /api/mappings", s.handleSaveMappings)
	s.mux.HandleFunc("POST /api/channels/manual", s.handleAddManualChannel)
	s.mux.HandleFunc("POST /api/channels/{guide_num}/primary", s.handleSetPrimaryID)
	s.mux.HandleFunc("DELETE /api/channels/{guide_num}/id/{ace_id}", s.handleDeleteSourceID)
	s.mux.HandleFunc("POST /api/test_channel/{chno}", s.handleTestChannel)

	s.mux.HandleFunc("GET /playlist.m3u", s.handlePlaylist)
	s.mux.HandleFunc("GET /epg.xml", s.handleEPGXML)
	s.mux.HandleFunc("GET /discover.json", s.handleHDHRDiscover)
	s.mux.HandleFunc("GET /lineup_status.json", s.handleHDHRLineupStatus)
	s.mux.HandleFunc("GET /lineup.json", s.handleHDHRLineup)
	s.mux.HandleFunc("POST /lineup.post", s.handleHDHRLineupPost)
	s.mux.HandleFunc("GET /auto/{id}", s.handleStream)
}

func isPublicPath(path string) bool {
	if path == "/login" || path == "/logout" || path == "/playlist.m3u" || path == "/epg.xml" ||
		path == "/discover.json" || path == "/lineup_status.json" || path == "/lineup.json" || path == "/lineup.post" {
		return true
	}
	if strings.HasPrefix(path, "/static/") || strings.HasPrefix(path, "/auto/") {
		return true
	}
	return false
}

func (s *Server) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/auto/") {
			w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
			w.Header().Set("Pragma", "no-cache")
			w.Header().Set("Expires", "0")
		}
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type,Authorization")
		w.Header().Set("Access-Control-Allow-Methods", "GET,PUT,POST,DELETE,OPTIONS")

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		if !isPublicPath(r.URL.Path) {
			cfg := s.cfgMgr.Load()
			if !isAuthenticated(r, cfg.SecretKey) {
				if strings.HasPrefix(r.URL.Path, "/api/") {
					writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "Unauthorized"})
					return
				}
				http.Redirect(w, r, "/login?next="+r.URL.RequestURI(), http.StatusFound)
				return
			}
		}

		next.ServeHTTP(w, r)
	})
}

type basePageData struct {
	Lang                string
	Path                string
	ResetPasswordActive bool
}

func (s *Server) baseData(cfg config.Config, r *http.Request) basePageData {
	resetEnv := strings.ToLower(strings.TrimSpace(os.Getenv("RESET_PASSWORD")))
	return basePageData{
		Lang:                cfg.Language(),
		Path:                r.URL.Path,
		ResetPasswordActive: resetEnv == "true" || resetEnv == "1" || resetEnv == "yes",
	}
}

func (s *Server) handleLoginPage(w http.ResponseWriter, r *http.Request) {
	cfg := s.cfgMgr.Load()
	s.tmpls.render(w, "login.html", map[string]any{
		"Lang":    cfg.Language(),
		"IsSetup": cfg.AdminPasswordHash == "",
		"Error":   "",
	})
}

func (s *Server) handleLoginPost(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Bad request", http.StatusBadRequest)
		return
	}
	cfg := s.cfgMgr.Load()
	lang := cfg.Language()
	isSetup := cfg.AdminPasswordHash == ""

	if isSetup {
		password := r.FormValue("password")
		confirm := r.FormValue("confirm_password")
		if password == "" || password != confirm {
			s.tmpls.render(w, "login.html", map[string]any{
				"Lang":    lang,
				"IsSetup": true,
				"Error":   i18n.GetText("Las contraseñas no coinciden", lang),
			})
			return
		}
		hash, err := HashPassword(password)
		if err != nil {
			http.Error(w, "Error hashing password", http.StatusInternalServerError)
			return
		}
		updated, err := s.cfgMgr.Update(func(c *config.Config) {
			c.AdminPasswordHash = hash
		})
		if err != nil {
			http.Error(w, "Error saving config", http.StatusInternalServerError)
			return
		}
		setSessionCookie(w, updated.SecretKey)
		http.Redirect(w, r, "/dashboard", http.StatusFound)
		return
	}

	password := r.FormValue("password")
	if CheckPassword(cfg.AdminPasswordHash, password) {
		setSessionCookie(w, cfg.SecretKey)
		next := r.URL.Query().Get("next")
		if next == "" || !strings.HasPrefix(next, "/") {
			next = "/dashboard"
		}
		http.Redirect(w, r, next, http.StatusFound)
		return
	}

	s.tmpls.render(w, "login.html", map[string]any{
		"Lang":    lang,
		"IsSetup": false,
		"Error":   i18n.GetText("Contraseña incorrecta", lang),
	})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	clearSessionCookie(w)
	http.Redirect(w, r, "/login", http.StatusFound)
}

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	cfg := s.cfgMgr.Load()
	s.tmpls.render(w, "dashboard.html", map[string]any{
		"Base":          s.baseData(cfg, r),
		"Lang":          cfg.Language(),
		"Channels":      len(s.chanStore.Channels()),
		"Sources":       len(cfg.Sources),
		"Streams":       s.chanStore.TotalStreams(),
		"ActiveClients": s.hub.ActiveClientCount(),
		"Settings":      cfg.Settings,
		"Host":          r.Host,
	})
}

func (s *Server) handleAPIStats(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.hub.GetStats())
}

func (s *Server) handleSourcesPage(w http.ResponseWriter, r *http.Request) {
	cfg := s.cfgMgr.Load()
	s.tmpls.render(w, "sources.html", map[string]any{
		"Base":    s.baseData(cfg, r),
		"Lang":    cfg.Language(),
		"Sources": cfg.Sources,
	})
}

func (s *Server) handleAddSource(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name            string `json:"name"`
		URL             string `json:"url"`
		Type            string `json:"type"`
		IntervalMinutes any    `json:"interval_minutes"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Invalid JSON"})
		return
	}
	if body.Name == "" {
		body.Name = "New Source"
	}
	if body.Type == "" {
		body.Type = "m3u"
	}
	interval := parseAnyInt(body.IntervalMinutes, 60)
	idBuf := make([]byte, 4)
	_, _ = rand.Read(idBuf)

	newSrc := config.Source{
		ID:              hex.EncodeToString(idBuf),
		Name:            body.Name,
		URL:             body.URL,
		Type:            body.Type,
		IntervalMinutes: interval,
		LastRun:         nil,
		Status:          "Pending",
	}
	_, _ = s.cfgMgr.Update(func(c *config.Config) {
		c.Sources = append(c.Sources, newSrc)
	})
	s.sched.Init()
	writeJSON(w, http.StatusOK, map[string]string{"status": "success"})
}

func (s *Server) handleDeleteSource(w http.ResponseWriter, r *http.Request) {
	sourceID := r.PathValue("source_id")
	_, _ = s.cfgMgr.Update(func(c *config.Config) {
		filtered := c.Sources[:0]
		for _, src := range c.Sources {
			if src.ID != sourceID {
				filtered = append(filtered, src)
			}
		}
		c.Sources = filtered
	})
	s.chanStore.DeleteSource(sourceID)
	s.sched.Init()
	writeJSON(w, http.StatusOK, map[string]string{"status": "success"})
}

func (s *Server) handleRefreshSource(w http.ResponseWriter, r *http.Request) {
	sourceID := r.PathValue("source_id")
	s.sched.FetchSource(sourceID, true)
	writeJSON(w, http.StatusOK, map[string]string{"status": "success"})
}

func (s *Server) handleChannelsPage(w http.ResponseWriter, r *http.Request) {
	cfg := s.cfgMgr.Load()
	s.tmpls.render(w, "channels.html", map[string]any{
		"Base":        s.baseData(cfg, r),
		"Lang":        cfg.Language(),
		"Channels":    s.chanStore.Channels(),
		"EPGChannels": s.chanStore.EPGChannels(),
		"ScanDB":      s.dbStore.GetAllScans(),
	})
}

type epgChannelView struct {
	channels.Channel
	Programmes []scraper.EPGProgramme
}

func (s *Server) handleEPGPage(w http.ResponseWriter, r *http.Request) {
	cfg := s.cfgMgr.Load()
	chs := s.chanStore.Channels()
	progs := s.chanStore.EPGProgrammes()

	grouped := make(map[string][]scraper.EPGProgramme)
	for _, p := range progs {
		if p.Channel != "" {
			grouped[p.Channel] = append(grouped[p.Channel], p)
		}
	}
	for cid := range grouped {
		sort.Slice(grouped[cid], func(i, j int) bool {
			return grouped[cid][i].StartTime < grouped[cid][j].StartTime
		})
	}

	var mapped []epgChannelView
	for _, c := range chs {
		if c.MappedEPGID != "" {
			mapped = append(mapped, epgChannelView{
				Channel:    c,
				Programmes: grouped[c.MappedEPGID],
			})
		}
	}

	s.tmpls.render(w, "epg.html", map[string]any{
		"Base":     s.baseData(cfg, r),
		"Lang":     cfg.Language(),
		"Channels": mapped,
	})
}

func (s *Server) handleSettingsPage(w http.ResponseWriter, r *http.Request) {
	cfg := s.cfgMgr.Load()
	s.tmpls.render(w, "settings.html", map[string]any{
		"Base":     s.baseData(cfg, r),
		"Lang":     cfg.Language(),
		"Settings": cfg.Settings,
	})
}

func (s *Server) handleSaveSettings(w http.ResponseWriter, r *http.Request) {
	var data map[string]any
	if err := json.NewDecoder(r.Body).Decode(&data); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Invalid JSON"})
		return
	}

	keys := []string{
		"scan_on_play_enabled",
		"placeholder_enabled",
		"scan_on_play_interval_minutes",
		"language",
		"hot_failover_enabled",
		"engine_idle_cleanup_enabled",
		"engine_idle_timeout",
		"auto_discard_enabled",
		"hdhr_enabled",
		"stream_timeout",
		"engine_live_buffer",
		"engine_disk_cache",
	}

	updated, err := s.cfgMgr.Update(func(c *config.Config) {
		for _, k := range keys {
			if v, ok := data[k]; ok {
				c.Settings[k] = v
			}
		}
		delete(c.Settings, "scan_adjacent_enabled")
		delete(c.Settings, "server_ram_buffer_mb")
	})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	_ = s.cfgMgr.WriteEngineArgs(updated)
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleEngineCleanup(w http.ResponseWriter, r *http.Request) {
	reclaimed := s.hub.CleanupAll()
	writeJSON(w, http.StatusOK, map[string]any{
		"status":             "ok",
		"reclaimed_sessions": reclaimed,
	})
}

func (s *Server) handleReorderChannels(w http.ResponseWriter, r *http.Request) {
	var body struct {
		CustomOrder []string `json:"custom_order"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Invalid JSON"})
		return
	}
	if body.CustomOrder != nil {
		_, _ = s.cfgMgr.Update(func(c *config.Config) {
			c.CustomOrder = body.CustomOrder
		})
		s.chanStore.Rebuild()
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleSaveMappings(w http.ResponseWriter, r *http.Request) {
	var data map[string]string
	if err := json.NewDecoder(r.Body).Decode(&data); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Invalid JSON"})
		return
	}
	_, _ = s.cfgMgr.Update(func(c *config.Config) {
		for chID, epgID := range data {
			if epgID != "" {
				c.Mappings[chID] = epgID
			} else {
				delete(c.Mappings, chID)
			}
		}
	})
	s.chanStore.Rebuild()
	writeJSON(w, http.StatusOK, map[string]string{"status": "success"})
}

var validAceIDRegex = regexp.MustCompile(`^[a-fA-F0-9]{40}$`)

func (s *Server) handleAddManualChannel(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ChannelName string `json:"channel_name"`
		AceID       string `json:"ace_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Invalid JSON"})
		return
	}
	if body.ChannelName == "" || body.AceID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Missing parameters"})
		return
	}
	if !validAceIDRegex.MatchString(body.AceID) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Invalid Ace ID format"})
		return
	}

	_, _ = s.cfgMgr.Update(func(c *config.Config) {
		exists := false
		for _, ms := range c.ManualSources {
			if ms.Title == body.ChannelName && ms.AceID == body.AceID {
				exists = true
				break
			}
		}
		if !exists {
			c.ManualSources = append(c.ManualSources, config.ManualSource{
				Title: body.ChannelName,
				AceID: body.AceID,
			})
		}
		filtered := c.BlacklistIDs[:0]
		for _, bid := range c.BlacklistIDs {
			if bid != body.AceID {
				filtered = append(filtered, bid)
			}
		}
		c.BlacklistIDs = filtered
	})
	s.chanStore.Rebuild()
	writeJSON(w, http.StatusOK, map[string]string{"status": "success"})
}

func (s *Server) handleSetPrimaryID(w http.ResponseWriter, r *http.Request) {
	guideNum := r.PathValue("guide_num")
	var body struct {
		AceID string `json:"ace_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Invalid JSON"})
		return
	}
	_, _ = s.cfgMgr.Update(func(c *config.Config) {
		c.PreferredIDs[guideNum] = body.AceID
	})
	s.chanStore.Rebuild()
	writeJSON(w, http.StatusOK, map[string]string{"status": "success"})
}

func (s *Server) handleDeleteSourceID(w http.ResponseWriter, r *http.Request) {
	guideNum := r.PathValue("guide_num")
	aceID := r.PathValue("ace_id")

	_, _ = s.cfgMgr.Update(func(c *config.Config) {
		found := false
		for _, bid := range c.BlacklistIDs {
			if bid == aceID {
				found = true
				break
			}
		}
		if !found {
			c.BlacklistIDs = append(c.BlacklistIDs, aceID)
		}
		if c.PreferredIDs[guideNum] == aceID {
			delete(c.PreferredIDs, guideNum)
		}
	})
	s.chanStore.Rebuild()
	writeJSON(w, http.StatusOK, map[string]string{"status": "success"})
}

func (s *Server) handleTestChannel(w http.ResponseWriter, r *http.Request) {
	chno := r.PathValue("chno")
	res := s.scanner.PerformChannelScan(r.Context(), chno, 0)
	if res.Error != "" && len(res.Results) == 0 {
		writeJSON(w, http.StatusNotFound, res)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

var (
	reTvgName    = regexp.MustCompile(`tvg-name="[^"]*"`)
	reTvgID      = regexp.MustCompile(`tvg-id="[^"]*"`)
	reTvgLogo    = regexp.MustCompile(`tvg-logo="[^"]*"`)
	reExtinfHead = regexp.MustCompile(`(#EXTINF:\s*[-\d]+)`)
)

func (s *Server) handlePlaylist(w http.ResponseWriter, r *http.Request) {
	chs := s.chanStore.Channels()
	var b strings.Builder
	b.WriteString("#EXTM3U\n")

	for _, c := range chs {
		extinf := c.Extinf
		if extinf == "" {
			extinf = fmt.Sprintf(`#EXTINF:-1 tvg-chno="%d",%s`, c.SequenceNumber, c.GuideName)
		} else if idx := strings.LastIndex(extinf, ","); idx >= 0 {
			extinf = extinf[:idx+1] + c.GuideName
		} else {
			extinf = extinf + "," + c.GuideName
		}

		if reTvgName.MatchString(extinf) {
			extinf = reTvgName.ReplaceAllString(extinf, fmt.Sprintf(`tvg-name="%s"`, c.GuideName))
		} else {
			extinf = replaceOnce(reExtinfHead, extinf, fmt.Sprintf(`$1 tvg-name="%s"`, c.GuideName))
		}

		if c.MappedEPGID != "" {
			if reTvgID.MatchString(extinf) {
				extinf = reTvgID.ReplaceAllString(extinf, fmt.Sprintf(`tvg-id="%s"`, c.MappedEPGID))
			} else {
				extinf = replaceOnce(reExtinfHead, extinf, fmt.Sprintf(`$1 tvg-id="%s"`, c.MappedEPGID))
			}

			if c.MappedEPGLogo != "" {
				if reTvgLogo.MatchString(extinf) {
					extinf = reTvgLogo.ReplaceAllString(extinf, fmt.Sprintf(`tvg-logo="%s"`, c.MappedEPGLogo))
				} else {
					extinf = replaceOnce(reExtinfHead, extinf, fmt.Sprintf(`$1 tvg-logo="%s"`, c.MappedEPGLogo))
				}
			}
		}

		b.WriteString(extinf)
		b.WriteByte('\n')
		b.WriteString(fmt.Sprintf("http://%s%s\n", r.Host, c.URL))
	}

	w.Header().Set("Content-Type", "audio/x-mpegurl")
	_, _ = w.Write([]byte(strings.TrimSuffix(b.String(), "\n")))
}

func replaceOnce(re *regexp.Regexp, src, repl string) string {
	loc := re.FindStringSubmatchIndex(src)
	if loc == nil {
		return src
	}
	expanded := re.ExpandString(nil, repl, src, loc)
	return src[:loc[0]] + string(expanded) + src[loc[1]:]
}

type xmlTVOut struct {
	XMLName    xml.Name          `xml:"tv"`
	Generator  string            `xml:"generator-info-name,attr"`
	Channels   []xmlChannelOut   `xml:"channel"`
	Programmes []xmlProgrammeOut `xml:"programme"`
}

type xmlChannelOut struct {
	ID          string       `xml:"id,attr"`
	DisplayName string       `xml:"display-name"`
	Icon        *xmlIconOut  `xml:"icon,omitempty"`
}

type xmlIconOut struct {
	Src string `xml:"src,attr"`
}

type xmlProgrammeOut struct {
	Start   string       `xml:"start,attr"`
	Stop    string       `xml:"stop,attr"`
	Channel string       `xml:"channel,attr"`
	Title   xmlLangText  `xml:"title"`
	Desc    *xmlLangText `xml:"desc,omitempty"`
}

type xmlLangText struct {
	Lang string `xml:"lang,attr,omitempty"`
	Text string `xml:",chardata"`
}

func (s *Server) handleEPGXML(w http.ResponseWriter, r *http.Request) {
	chs := s.chanStore.Channels()
	progs := s.chanStore.EPGProgrammes()

	grouped := make(map[string][]scraper.EPGProgramme)
	for _, p := range progs {
		if p.Channel != "" {
			grouped[p.Channel] = append(grouped[p.Channel], p)
		}
	}

	out := xmlTVOut{
		Generator: "AceStream Hub",
	}

	now := time.Now().UTC()
	dummyStart := now.Format("20060102000000 +0000")
	dummyStop := now.Add(24 * time.Hour).Format("20060102000000 +0000")

	for _, c := range chs {
		chID := c.MappedEPGID
		if chID == "" {
			chID = c.GuideNumber
		}
		xc := xmlChannelOut{
			ID:          chID,
			DisplayName: c.GuideName,
		}
		if c.MappedEPGLogo != "" {
			xc.Icon = &xmlIconOut{Src: c.MappedEPGLogo}
		}
		out.Channels = append(out.Channels, xc)

		chProgs := grouped[c.MappedEPGID]
		if len(chProgs) == 0 {
			out.Programmes = append(out.Programmes, xmlProgrammeOut{
				Start:   dummyStart,
				Stop:    dummyStop,
				Channel: chID,
				Title:   xmlLangText{Lang: "es", Text: "Programación de " + c.GuideName},
				Desc:    &xmlLangText{Lang: "es", Text: "Programación no disponible para este canal."},
			})
			continue
		}

		for _, p := range chProgs {
			sStr := time.Unix(p.StartTime, 0).UTC().Format("20060102150405 +0000")
			eStr := time.Unix(p.StopTime, 0).UTC().Format("20060102150405 +0000")
			title := p.Title
			if title == "" {
				title = "Unknown"
			}
			xp := xmlProgrammeOut{
				Start:   sStr,
				Stop:    eStr,
				Channel: chID,
				Title:   xmlLangText{Text: title},
			}
			if p.Desc != "" {
				xp.Desc = &xmlLangText{Text: p.Desc}
			}
			out.Programmes = append(out.Programmes, xp)
		}
	}

	xmlBytes, err := xml.Marshal(out)
	if err != nil {
		http.Error(w, "Error generating EPG XML", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/xml")
	_, _ = w.Write([]byte(xml.Header))
	_, _ = w.Write(xmlBytes)
}

func (s *Server) handleHDHRDiscover(w http.ResponseWriter, r *http.Request) {
	if !s.cfgMgr.Load().HDHREnabled() {
		http.Error(w, "HDHomeRun emulator is disabled", http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"FriendlyName":    "AceStream Hub HDHR",
		"Manufacturer":    "Silicondust",
		"ModelNumber":     "HDTC-2US",
		"FirmwareName":    "hdhomeruntc_atsc",
		"TunerCount":      4,
		"FirmwareVersion": "20150826",
		"DeviceID":        "12345678",
		"DeviceAuth":      "test1234",
		"BaseURL":         fmt.Sprintf("http://%s", r.Host),
		"LineupURL":       fmt.Sprintf("http://%s/lineup.json", r.Host),
	})
}

func (s *Server) handleHDHRLineupStatus(w http.ResponseWriter, r *http.Request) {
	if !s.cfgMgr.Load().HDHREnabled() {
		http.Error(w, "HDHomeRun emulator is disabled", http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ScanInProgress": 0,
		"ScanPossible":   1,
		"Source":         "Cable",
		"SourceList":     []string{"Cable"},
	})
}

func (s *Server) handleHDHRLineup(w http.ResponseWriter, r *http.Request) {
	if !s.cfgMgr.Load().HDHREnabled() {
		http.Error(w, "HDHomeRun emulator is disabled", http.StatusNotFound)
		return
	}
	chs := s.chanStore.Channels()
	lineup := make([]map[string]string, 0, len(chs))
	for _, c := range chs {
		lineup = append(lineup, map[string]string{
			"GuideNumber": c.GuideNumber,
			"GuideName":   c.GuideName,
			"URL":         fmt.Sprintf("http://%s%s", r.Host, c.URL),
		})
	}
	writeJSON(w, http.StatusOK, lineup)
}

func (s *Server) handleHDHRLineupPost(w http.ResponseWriter, r *http.Request) {
	if !s.cfgMgr.Load().HDHREnabled() {
		http.Error(w, "HDHomeRun emulator is disabled", http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	rawID := r.PathValue("id")
	channelID := strings.TrimPrefix(rawID, "v")
	if channelID == rawID {
		http.NotFound(w, r)
		return
	}

	channel, ok := s.chanStore.FindChannel(channelID)
	if !ok {
		http.Error(w, "Channel not found", http.StatusNotFound)
		return
	}

	clientIP := r.Header.Get("X-Forwarded-For")
	if clientIP != "" {
		if idx := strings.IndexByte(clientIP, ','); idx >= 0 {
			clientIP = strings.TrimSpace(clientIP[:idx])
		} else {
			clientIP = strings.TrimSpace(clientIP)
		}
	} else {
		clientIP = r.RemoteAddr
		if idx := strings.LastIndexByte(clientIP, ':'); idx > 0 {
			clientIP = clientIP[:idx]
		}
	}
	userAgent := r.UserAgent()
	if userAgent == "" {
		userAgent = "Unknown Player"
	}
	randBytes := make([]byte, 3)
	_, _ = rand.Read(randBytes)
	clientID := fmt.Sprintf("%s_%s", clientIP, hex.EncodeToString(randBytes))

	sub, err := s.hub.Subscribe(channel, clientID, clientIP, userAgent)
	if err != nil {
		http.Error(w, "Channel stream unavailable: all sources failed", http.StatusServiceUnavailable)
		return
	}
	defer sub.Release()

	w.Header().Set("Content-Type", "video/mp2t")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)

	for {
		select {
		case <-r.Context().Done():
			return
		case chunk, open := <-sub.Ch:
			if !open {
				return
			}
			if _, writeErr := w.Write(chunk); writeErr != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func parseAnyInt(v any, def int) int {
	if v == nil {
		return def
	}
	switch t := v.(type) {
	case float64:
		return int(t)
	case int:
		return t
	case string:
		if n, err := strconv.Atoi(strings.TrimSpace(t)); err == nil {
			return n
		}
	}
	return def
}
