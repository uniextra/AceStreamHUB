package channels

import (
	"crypto/md5"
	"math"
	"math/big"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"unicode"

	"acestreamhub/internal/config"
	"acestreamhub/internal/db"
	"acestreamhub/internal/scraper"
)

// Channel represents a unified channel exposed via playlist.m3u, HDHR, and UI.
type Channel struct {
	GuideNumber    string   `json:"GuideNumber"`
	GuideName      string   `json:"GuideName"`
	URL            string   `json:"URL"`
	AceIDs         []string `json:"ace_ids"`
	Extinf         string   `json:"extinf,omitempty"`
	MappedEPGID    string   `json:"mapped_epg_id"`
	MappedEPGLogo  string   `json:"mapped_epg_logo"`
	PreferredID    string   `json:"preferred_id,omitempty"`
	SequenceNumber int      `json:"SequenceNumber"`
}

// Store holds in-memory scraped sources, EPG feeds, and unified channels.
type Store struct {
	mu           sync.RWMutex
	cfgMgr       *config.Manager
	dbStore      *db.Store
	rawSources   map[string][]scraper.RawChannel
	epgSources   map[string][]scraper.EPGChannel
	epgProgsSrc  map[string][]scraper.EPGProgramme
	epgChannels  []scraper.EPGChannel
	epgProgs     []scraper.EPGProgramme
	channels     []Channel
}

var trailingQualityRegex = regexp.MustCompile(`(?i)\s+(1080p|720p|4k|hd|fhd|sd|hevc|h265|x265)\s*$`)

// NewStore creates a thread-safe Store backed by cfgMgr and dbStore.
func NewStore(cfgMgr *config.Manager, dbStore *db.Store) *Store {
	return &Store{
		cfgMgr:      cfgMgr,
		dbStore:     dbStore,
		rawSources:  make(map[string][]scraper.RawChannel),
		epgSources:  make(map[string][]scraper.EPGChannel),
		epgProgsSrc: make(map[string][]scraper.EPGProgramme),
	}
}

// GetStableID computes a deterministic 4-digit channel number identical to Python's MD5 formula.
func GetStableID(name string) string {
	sum := md5.Sum([]byte(name))
	val := new(big.Int).SetBytes(sum[:])
	mod := new(big.Int).Mod(val, big.NewInt(9000))
	return strconv.FormatInt(mod.Int64()+1000, 10)
}

// SetRawSource stores channels scraped from a channel source and rebuilds the catalog.
func (s *Store) SetRawSource(sourceID string, items []scraper.RawChannel) {
	s.mu.Lock()
	s.rawSources[sourceID] = items
	s.mu.Unlock()
	s.Rebuild()
}

// DeleteSource removes any cached raw or EPG data for sourceID and rebuilds.
func (s *Store) DeleteSource(sourceID string) {
	s.mu.Lock()
	delete(s.rawSources, sourceID)
	_, hadEPG := s.epgSources[sourceID]
	delete(s.epgSources, sourceID)
	delete(s.epgProgsSrc, sourceID)
	if hadEPG {
		s.mergeEPGUnlocked()
	}
	s.mu.Unlock()
	s.Rebuild()
}

// SetEPGSource stores parsed EPG data for sourceID, merges all EPG feeds, and rebuilds.
func (s *Store) SetEPGSource(sourceID string, chs []scraper.EPGChannel, progs []scraper.EPGProgramme) (int, int) {
	s.mu.Lock()
	s.epgSources[sourceID] = chs
	s.epgProgsSrc[sourceID] = progs
	s.mergeEPGUnlocked()
	totalCh := len(s.epgChannels)
	totalProg := len(s.epgProgs)
	s.mu.Unlock()
	s.Rebuild()
	return totalCh, totalProg
}

func (s *Store) mergeEPGUnlocked() {
	var mergedCh []scraper.EPGChannel
	for _, list := range s.epgSources {
		mergedCh = append(mergedCh, list...)
	}
	sort.Slice(mergedCh, func(i, j int) bool {
		return mergedCh[i].Name < mergedCh[j].Name
	})
	s.epgChannels = mergedCh

	var mergedProg []scraper.EPGProgramme
	for _, list := range s.epgProgsSrc {
		mergedProg = append(mergedProg, list...)
	}
	s.epgProgs = mergedProg
}

// Channels returns a snapshot copy of the unified channel list.
func (s *Store) Channels() []Channel {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Channel, len(s.channels))
	for i, c := range s.channels {
		cp := c
		cp.AceIDs = append([]string(nil), c.AceIDs...)
		out[i] = cp
	}
	return out
}

// FindChannel looks up a channel by GuideNumber.
func (s *Store) FindChannel(guideNum string) (Channel, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, c := range s.channels {
		if c.GuideNumber == guideNum {
			cp := c
			cp.AceIDs = append([]string(nil), c.AceIDs...)
			return cp, true
		}
	}
	return Channel{}, false
}

// TotalStreams returns the sum of Ace IDs across all unified channels.
func (s *Store) TotalStreams() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	total := 0
	for _, c := range s.channels {
		total += len(c.AceIDs)
	}
	return total
}

// EPGChannels returns a snapshot copy of all merged EPG channels.
func (s *Store) EPGChannels() []scraper.EPGChannel {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]scraper.EPGChannel, len(s.epgChannels))
	copy(out, s.epgChannels)
	return out
}

// EPGProgrammes returns a snapshot copy of all merged EPG programmes.
func (s *Store) EPGProgrammes() []scraper.EPGProgramme {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]scraper.EPGProgramme, len(s.epgProgs))
	copy(out, s.epgProgs)
	return out
}

// Rebuild merges raw and manual sources, ranks Ace IDs, auto-maps EPG, and applies custom order.
func (s *Store) Rebuild() int {
	cfg := s.cfgMgr.Load()
	scans := map[string]db.ScanRecord{}
	if s.dbStore != nil {
		scans = s.dbStore.GetAllScans()
	}

	blacklist := make(map[string]struct{}, len(cfg.BlacklistIDs))
	for _, id := range cfg.BlacklistIDs {
		blacklist[id] = struct{}{}
	}

	type groupedEntry struct {
		aceIDs        []string
		seenIDs       map[string]struct{}
		extinf        string
		title         string
		providedTVGID string
		providedLogo  string
	}
	grouped := make(map[string]*groupedEntry)

	addItem := func(baseTitle, aceID, extinf, tvgID, logo string) {
		if aceID == "" {
			return
		}
		if _, blocked := blacklist[aceID]; blocked {
			return
		}
		g, exists := grouped[baseTitle]
		if !exists {
			g = &groupedEntry{
				seenIDs:       make(map[string]struct{}),
				title:         baseTitle,
				extinf:        extinf,
				providedTVGID: tvgID,
				providedLogo:  logo,
			}
			grouped[baseTitle] = g
		}
		if _, dup := g.seenIDs[aceID]; !dup {
			g.seenIDs[aceID] = struct{}{}
			g.aceIDs = append(g.aceIDs, aceID)
		}
		if g.extinf == "" && extinf != "" {
			g.extinf = extinf
		}
		if g.providedTVGID == "" && tvgID != "" {
			g.providedTVGID = tvgID
		}
		if g.providedLogo == "" && logo != "" {
			g.providedLogo = logo
		}
	}

	s.mu.RLock()
	for _, items := range s.rawSources {
		for _, item := range items {
			baseTitle := strings.TrimSpace(strings.TrimRight(item.Title, "* "))
			if idx := strings.Index(baseTitle, "-->"); idx >= 0 {
				baseTitle = strings.TrimSpace(baseTitle[:idx])
			}
			baseTitle = strings.TrimSpace(trailingQualityRegex.ReplaceAllString(baseTitle, ""))
			addItem(baseTitle, item.AceID, item.Extinf, item.ProvidedTVGID, item.ProvidedLogo)
		}
	}
	epgCopy := make([]scraper.EPGChannel, len(s.epgChannels))
	copy(epgCopy, s.epgChannels)
	s.mu.RUnlock()

	for _, ms := range cfg.ManualSources {
		extinf := ""
		if ms.Extinf != nil {
			extinf = *ms.Extinf
		}
		addItem(strings.TrimSpace(ms.Title), ms.AceID, extinf, "", "")
	}

	// Deterministic processing order matching sorted keys avoids hash collision nondeterminism.
	titles := make([]string, 0, len(grouped))
	for title := range grouped {
		titles = append(titles, title)
	}
	sort.Strings(titles)

	usedChnos := make(map[string]struct{}, len(titles))
	unified := make([]Channel, 0, len(titles))

	for _, baseTitle := range titles {
		data := grouped[baseTitle]
		chno := GetStableID(baseTitle)
		for {
			if _, used := usedChnos[chno]; !used {
				break
			}
			n, _ := strconv.Atoi(chno)
			chno = strconv.Itoa(n + 1)
		}
		usedChnos[chno] = struct{}{}

		epgID := cfg.Mappings[chno]
		if epgID == "" && data.providedTVGID != "" {
			epgID = data.providedTVGID
		}

		prefID := cfg.PreferredIDs[chno]
		aceIDs := rankAceIDs(data.aceIDs, prefID, scans)

		unified = append(unified, Channel{
			GuideNumber:   chno,
			GuideName:     baseTitle,
			URL:           "/auto/v" + chno,
			AceIDs:        aceIDs,
			Extinf:        data.extinf,
			MappedEPGID:   epgID,
			MappedEPGLogo: data.providedLogo,
			PreferredID:   prefID,
		})
	}

	sort.Slice(unified, func(i, j int) bool {
		return unified[i].GuideName < unified[j].GuideName
	})

	newMappings := make(map[string]string)
	if len(epgCopy) > 0 {
		epgNames := make(map[string]string, len(epgCopy))
		epgLogos := make(map[string]string, len(epgCopy))
		epgCleanList := make([]string, 0, len(epgCopy))
		for _, e := range epgCopy {
			epgLogos[e.ID] = e.Logo
			cleaned := cleanChannelName(e.Name)
			if cleaned != "" {
				if _, exists := epgNames[cleaned]; !exists {
					epgNames[cleaned] = e.ID
					epgCleanList = append(epgCleanList, cleaned)
				}
			}
		}

		for i := range unified {
			if unified[i].MappedEPGID == "" {
				cClean := cleanChannelName(unified[i].GuideName)
				if cClean != "" {
					bestMatch, bestScore := bestTokenSetMatch(cClean, epgCleanList)
					if bestScore >= 85 {
						mappedID := epgNames[bestMatch]
						unified[i].MappedEPGID = mappedID
						newMappings[unified[i].GuideNumber] = mappedID
					}
				}
			}
			if unified[i].MappedEPGID != "" && unified[i].MappedEPGLogo == "" {
				unified[i].MappedEPGLogo = epgLogos[unified[i].MappedEPGID]
			}
		}
	}

	if len(newMappings) > 0 {
		_, _ = s.cfgMgr.Update(func(c *config.Config) {
			for k, v := range newMappings {
				if c.Mappings[k] == "" {
					c.Mappings[k] = v
				}
			}
		})
	}

	orderIdx := make(map[string]int, len(cfg.CustomOrder))
	for idx, chno := range cfg.CustomOrder {
		orderIdx[chno] = idx
	}
	sort.SliceStable(unified, func(i, j int) bool {
		idxI, okI := orderIdx[unified[i].GuideNumber]
		idxJ, okJ := orderIdx[unified[j].GuideNumber]
		if okI && okJ {
			return idxI < idxJ
		}
		if okI != okJ {
			return okI
		}
		return unified[i].GuideName < unified[j].GuideName
	})

	for i := range unified {
		unified[i].SequenceNumber = i + 1
	}

	s.mu.Lock()
	s.channels = unified
	s.mu.Unlock()
	return len(unified)
}

func rankAceIDs(ids []string, prefID string, scans map[string]db.ScanRecord) []string {
	out := append([]string(nil), ids...)
	sort.Slice(out, func(i, j int) bool {
		if out[i] == prefID {
			return true
		}
		if out[j] == prefID {
			return false
		}
		si, oki := scans[out[i]]
		sj, okj := scans[out[j]]
		scoreI := scanRankScore(si, oki)
		scoreJ := scanRankScore(sj, okj)
		if scoreI != scoreJ {
			return scoreI > scoreJ
		}
		return out[i] < out[j]
	})
	return out
}

func scanRankScore(rec db.ScanRecord, ok bool) int {
	if !ok {
		return 0
	}
	if !rec.Success {
		return -1000 - rec.ConsecutiveFails
	}
	score := 100
	switch strings.ToUpper(rec.Resolution) {
	case "FHD":
		score += 1000
	case "HD":
		score += 500
	}
	if peers, err := strconv.Atoi(strings.TrimSpace(rec.Peers)); err == nil {
		score += peers
	}
	return score
}

var cleanQualityWordRegex = regexp.MustCompile(`\b(hd|fhd|4k|1080p|720p)\b`)

func cleanChannelName(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteByte(' ')
		}
	}
	cleaned := cleanQualityWordRegex.ReplaceAllString(b.String(), "")
	return strings.TrimSpace(cleaned)
}

func bestTokenSetMatch(query string, choices []string) (string, int) {
	bestChoice := ""
	bestScore := -1
	for _, candidate := range choices {
		score := TokenSetRatio(query, candidate)
		if score > bestScore {
			bestScore = score
			bestChoice = candidate
		}
	}
	return bestChoice, bestScore
}

// TokenSetRatio approximates thefuzz.fuzz.token_set_ratio using SequenceMatcher-style ratio.
func TokenSetRatio(s1, s2 string) int {
	t1 := tokenizeNormalized(s1)
	t2 := tokenizeNormalized(s2)
	if len(t1) == 0 && len(t2) == 0 {
		return 100
	}
	if len(t1) == 0 || len(t2) == 0 {
		return 0
	}

	set1 := make(map[string]struct{}, len(t1))
	for _, tok := range t1 {
		set1[tok] = struct{}{}
	}
	set2 := make(map[string]struct{}, len(t2))
	for _, tok := range t2 {
		set2[tok] = struct{}{}
	}

	var inter, diff1, diff2 []string
	for tok := range set1 {
		if _, ok := set2[tok]; ok {
			inter = append(inter, tok)
		} else {
			diff1 = append(diff1, tok)
		}
	}
	for tok := range set2 {
		if _, ok := set1[tok]; !ok {
			diff2 = append(diff2, tok)
		}
	}

	sort.Strings(inter)
	sort.Strings(diff1)
	sort.Strings(diff2)

	sortedInter := strings.Join(inter, " ")
	combined1 := strings.TrimSpace(strings.Join(append(append([]string{}, inter...), diff1...), " "))
	combined2 := strings.TrimSpace(strings.Join(append(append([]string{}, inter...), diff2...), " "))

	if sortedInter == "" {
		return sequenceRatio(combined1, combined2)
	}

	maxRatio := sequenceRatio(sortedInter, combined1)
	if r := sequenceRatio(sortedInter, combined2); r > maxRatio {
		maxRatio = r
	}
	if r := sequenceRatio(combined1, combined2); r > maxRatio {
		maxRatio = r
	}
	return maxRatio
}

func tokenizeNormalized(s string) []string {
	fields := strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	return fields
}

func sequenceRatio(a, b string) int {
	ra := []rune(a)
	rb := []rune(b)
	total := len(ra) + len(rb)
	if total == 0 {
		return 100
	}
	matched := countMatchingChars(ra, rb)
	return int(math.Round(200.0 * float64(matched) / float64(total)))
}

func countMatchingChars(a, b []rune) int {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	var bestLen, bestA, bestB int
	dp := make([]int, len(b)+1)
	for i := 1; i <= len(a); i++ {
		prev := 0
		for j := 1; j <= len(b); j++ {
			cur := dp[j]
			if a[i-1] == b[j-1] {
				dp[j] = prev + 1
				if dp[j] > bestLen {
					bestLen = dp[j]
					bestA = i - bestLen
					bestB = j - bestLen
				}
			} else {
				dp[j] = 0
			}
			prev = cur
		}
	}
	if bestLen == 0 {
		return 0
	}
	return bestLen + countMatchingChars(a[:bestA], b[:bestB]) + countMatchingChars(a[bestA+bestLen:], b[bestB+bestLen:])
}
