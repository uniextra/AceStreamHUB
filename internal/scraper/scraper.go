package scraper

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"

	"golang.org/x/net/html"
)

// RawChannel represents a scraped channel entry before unification.
type RawChannel struct {
	Title         string `json:"title"`
	AceID         string `json:"ace_id"`
	Extinf        string `json:"extinf,omitempty"`
	ProvidedTVGID string `json:"provided_tvg_id,omitempty"`
	ProvidedLogo  string `json:"provided_logo,omitempty"`
}

// EPGChannel represents a channel declared inside an XMLTV feed.
type EPGChannel struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Logo string `json:"logo"`
}

// EPGProgramme represents a scheduled broadcast window from an XMLTV feed.
type EPGProgramme struct {
	Channel   string `json:"channel"`
	StartTime int64  `json:"start_time"`
	StopTime  int64  `json:"stop_time"`
	Title     string `json:"title"`
	Desc      string `json:"desc"`
}

var aceHashRegex = regexp.MustCompile(`([a-fA-F0-9]{40})`)

func resolveIPFS(rawURL string) string {
	if strings.Contains(rawURL, ".ipns.inbrowser.link") {
		if parts := strings.SplitN(rawURL, "://", 2); len(parts) == 2 {
			cid := strings.SplitN(parts[1], ".ipns", 2)[0]
			if pathParts := strings.SplitN(rawURL, ".link", 2); len(pathParts) == 2 {
				resolved := fmt.Sprintf("https://%s.ipns.dweb.link%s", cid, pathParts[1])
				log.Printf("Resolved IPNS URL to: %s", resolved)
				return resolved
			}
		}
	}
	if strings.Contains(rawURL, ".ipfs.inbrowser.link") {
		if parts := strings.SplitN(rawURL, "://", 2); len(parts) == 2 {
			cid := strings.SplitN(parts[1], ".ipfs", 2)[0]
			if pathParts := strings.SplitN(rawURL, ".link", 2); len(pathParts) == 2 {
				resolved := fmt.Sprintf("https://%s.ipfs.dweb.link%s", cid, pathParts[1])
				log.Printf("Resolved IPFS URL to: %s", resolved)
				return resolved
			}
		}
	}
	return rawURL
}

func httpGetBytes(targetURL string, timeout time.Duration, userAgent string) ([]byte, error) {
	client := &http.Client{Timeout: timeout}
	req, err := http.NewRequest(http.MethodGet, targetURL, nil)
	if err != nil {
		return nil, err
	}
	if userAgent != "" {
		req.Header.Set("User-Agent", userAgent)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("HTTP %d from %s", resp.StatusCode, targetURL)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	// Transparently decompress raw .gz payloads if returned without Content-Encoding header.
	if len(body) >= 2 && body[0] == 0x1f && body[1] == 0x8b {
		gz, err := gzip.NewReader(bytes.NewReader(body))
		if err == nil {
			defer gz.Close()
			if unzipped, err := io.ReadAll(gz); err == nil {
				return unzipped, nil
			}
		}
	}
	return body, nil
}

// ParseM3U downloads or reads an M3U playlist and extracts AceStream entries.
func ParseM3U(urlOrPath string) ([]RawChannel, error) {
	log.Printf("Parsing M3U from %s", urlOrPath)
	var content string
	if strings.HasPrefix(urlOrPath, "http") {
		target := resolveIPFS(urlOrPath)
		data, err := httpGetBytes(target, 10*time.Second, "Mozilla/5.0")
		if err != nil {
			log.Printf("M3U Parse Error: %v", err)
			return nil, err
		}
		content = string(data)
	} else {
		data, err := os.ReadFile(urlOrPath)
		if err != nil {
			log.Printf("M3U Parse Error: %v", err)
			return nil, err
		}
		content = string(data)
	}

	var channels []RawChannel
	var currentTitle string
	var currentExtinf string

	for _, rawLine := range strings.Split(content, "\n") {
		line := strings.TrimSpace(rawLine)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "#EXTINF") {
			currentExtinf = line
			if parts := strings.SplitN(line, ",", 2); len(parts) > 1 && strings.TrimSpace(parts[1]) != "" {
				currentTitle = strings.TrimSpace(parts[1])
			} else {
				currentTitle = fmt.Sprintf("Channel %d", len(channels)+1)
			}
			continue
		}
		if !strings.HasPrefix(line, "#") {
			var aceID string
			if strings.HasPrefix(line, "acestream://") {
				aceID = strings.TrimSpace(strings.TrimPrefix(line, "acestream://"))
			} else if match := aceHashRegex.FindStringSubmatch(line); len(match) > 1 {
				aceID = match[1]
			}
			if aceID != "" {
				title := currentTitle
				if title == "" {
					title = fmt.Sprintf("Channel %d", len(channels)+1)
				}
				channels = append(channels, RawChannel{
					Title:  title,
					AceID:  aceID,
					Extinf: currentExtinf,
				})
			}
			currentTitle = ""
			currentExtinf = ""
		}
	}
	return channels, nil
}

// ScrapeURL downloads an HTML page and extracts <a href="acestream://..."> links.
func ScrapeURL(rawURL string) ([]RawChannel, error) {
	log.Printf("Scraping from %s", rawURL)
	data, err := httpGetBytes(rawURL, 10*time.Second, "Mozilla/5.0")
	if err != nil {
		log.Printf("Scrape Error: %v", err)
		return nil, err
	}

	doc, err := html.Parse(bytes.NewReader(data))
	if err != nil {
		log.Printf("Scrape HTML Parse Error: %v", err)
		return nil, err
	}

	var channels []RawChannel
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == "a" {
			var href string
			for _, attr := range n.Attr {
				if attr.Key == "href" {
					href = attr.Val
					break
				}
			}
			if strings.HasPrefix(href, "acestream://") {
				aceID := strings.TrimSpace(strings.TrimPrefix(href, "acestream://"))
				title := strings.TrimSpace(extractNodeText(n))
				if title == "" {
					title = fmt.Sprintf("Scraped Channel %d", len(channels)+1)
				}
				channels = append(channels, RawChannel{
					Title: title,
					AceID: aceID,
				})
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	return channels, nil
}

func extractNodeText(n *html.Node) string {
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(cur *html.Node) {
		if cur.Type == html.TextNode {
			b.WriteString(cur.Data)
		}
		for c := cur.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return b.String()
}

type jsonFeedPayload struct {
	Hashes []struct {
		Title string `json:"title"`
		Hash  string `json:"hash"`
		TVGID string `json:"tvg_id"`
		Logo  string `json:"logo"`
	} `json:"hashes"`
}

// ParseJSON fetches a JSON hashes feed and extracts channel entries.
func ParseJSON(rawURL string) ([]RawChannel, error) {
	log.Printf("Fetching JSON from %s", rawURL)
	data, err := httpGetBytes(rawURL, 15*time.Second, "Mozilla/5.0")
	if err != nil {
		log.Printf("JSON Parse Error: %v", err)
		return nil, err
	}

	var payload jsonFeedPayload
	if err := json.Unmarshal(data, &payload); err != nil {
		log.Printf("JSON Decode Error: %v", err)
		return nil, err
	}

	channels := make([]RawChannel, 0, len(payload.Hashes))
	for _, item := range payload.Hashes {
		title := item.Title
		if title == "" {
			title = "Unknown"
		}
		channels = append(channels, RawChannel{
			Title:         title,
			AceID:         item.Hash,
			ProvidedTVGID: item.TVGID,
			ProvidedLogo:  item.Logo,
		})
	}
	return channels, nil
}

type xmlTVRoot struct {
	Channels []struct {
		ID          string   `xml:"id,attr"`
		DisplayName []string `xml:"display-name"`
		Icon        struct {
			Src string `xml:"src,attr"`
		} `xml:"icon"`
	} `xml:"channel"`
	Programmes []struct {
		Start   string `xml:"start,attr"`
		Stop    string `xml:"stop,attr"`
		Channel string `xml:"channel,attr"`
		Title   string `xml:"title"`
		Desc    string `xml:"desc"`
	} `xml:"programme"`
}

// ParseEPG downloads an XMLTV guide and keeps programmes active in the next 48 hours.
func ParseEPG(rawURL string) ([]EPGChannel, []EPGProgramme, error) {
	log.Printf("Fetching EPG from %s", rawURL)
	data, err := httpGetBytes(rawURL, 30*time.Second, "Mozilla/5.0")
	if err != nil {
		log.Printf("Error fetching EPG: %v", err)
		return nil, nil, err
	}

	var root xmlTVRoot
	if err := xml.Unmarshal(data, &root); err != nil {
		log.Printf("Error parsing EPG XML: %v", err)
		return nil, nil, err
	}

	now := time.Now().UTC()
	maxTime := now.Add(48 * time.Hour)

	channels := make([]EPGChannel, 0, len(root.Channels))
	for _, ch := range root.Channels {
		name := ch.ID
		for _, dn := range ch.DisplayName {
			if strings.TrimSpace(dn) != "" {
				name = strings.TrimSpace(dn)
				break
			}
		}
		channels = append(channels, EPGChannel{
			ID:   ch.ID,
			Name: name,
			Logo: ch.Icon.Src,
		})
	}

	var programmes []EPGProgramme
	for _, prog := range root.Programmes {
		startDT, err1 := parseXMLTVTime(prog.Start)
		stopDT, err2 := parseXMLTVTime(prog.Stop)
		if err1 != nil || err2 != nil {
			continue
		}
		if stopDT.After(now) && stopDT.Before(maxTime) {
			title := prog.Title
			if title == "" {
				title = "Unknown"
			}
			programmes = append(programmes, EPGProgramme{
				Channel:   prog.Channel,
				StartTime: startDT.Unix(),
				StopTime:  stopDT.Unix(),
				Title:     title,
				Desc:      prog.Desc,
			})
		}
	}
	return channels, programmes, nil
}

func parseXMLTVTime(val string) (time.Time, error) {
	val = strings.TrimSpace(val)
	layouts := []string{
		"20060102150405 -0700",
		"20060102150405-0700",
		"20060102150405",
	}
	for _, layout := range layouts {
		if t, err := time.Parse(layout, val); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("invalid XMLTV timestamp: %q", val)
}
