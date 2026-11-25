package web

import (
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"net/http"
	"strconv"

	"acestreamhub/internal/channels"
	"acestreamhub/internal/i18n"
)

type sourceColor struct {
	Bg   string
	Text string
	Dot  string
}

var sourceColors = map[string]sourceColor{
	"m3u":    {Bg: "bg-primary/10 border-primary/30", Text: "text-primary", Dot: "bg-primary"},
	"json":   {Bg: "bg-secondary-container/10 border-secondary-container/30", Text: "text-secondary-fixed-dim", Dot: "bg-secondary-container"},
	"epg":    {Bg: "bg-green-500/10 border-green-500/30", Text: "text-green-400", Dot: "bg-green-500"},
	"scrape": {Bg: "bg-orange-500/10 border-orange-500/30", Text: "text-orange-400", Dot: "bg-orange-500"},
}

type templateSet struct {
	pages map[string]*template.Template
	login *template.Template
}

func loadTemplates(fsys fs.FS) (*templateSet, error) {
	funcs := template.FuncMap{
		"t": func(lang, key string) string {
			return i18n.GetText(key, lang)
		},
		"slice8": first8,
		"last4":  last4,
		"fmt1":   fmtFloat1,
		"mappedCount": func(chs []channels.Channel) int {
			cnt := 0
			for _, c := range chs {
				if c.MappedEPGID != "" {
					cnt++
				}
			}
			return cnt
		},
		"settingBool": func(m map[string]any, key string, def bool) bool {
			if m == nil {
				return def
			}
			v, ok := m[key]
			if !ok || v == nil {
				return def
			}
			if b, ok := v.(bool); ok {
				return b
			}
			return def
		},
		"settingInt": func(m map[string]any, key string, def int) int {
			if m == nil {
				return def
			}
			v, ok := m[key]
			if !ok || v == nil {
				return def
			}
			switch t := v.(type) {
			case int:
				return t
			case int64:
				return int(t)
			case float64:
				return int(t)
			case string:
				if n, err := strconv.Atoi(t); err == nil {
					return n
				}
			}
			return def
		},
		"settingStr": func(m map[string]any, key string, def string) string {
			if m == nil {
				return def
			}
			if s, ok := m[key].(string); ok && s != "" {
				return s
			}
			return def
		},
		"srcColor": func(typ string) sourceColor {
			if c, ok := sourceColors[typ]; ok {
				return c
			}
			return sourceColors["m3u"]
		},
	}

	loginTmpl, err := template.New("login.html").Funcs(funcs).ParseFS(fsys, "web/templates/login.html")
	if err != nil {
		return nil, fmt.Errorf("parse login.html: %w", err)
	}

	pageNames := []string{
		"dashboard.html",
		"channels.html",
		"epg.html",
		"sources.html",
		"settings.html",
	}

	pages := make(map[string]*template.Template, len(pageNames))
	for _, name := range pageNames {
		tmpl, err := template.New(name).Funcs(funcs).ParseFS(
			fsys,
			"web/templates/base.html",
			"web/templates/"+name,
		)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", name, err)
		}
		pages[name] = tmpl
	}

	return &templateSet{
		pages: pages,
		login: loginTmpl,
	}, nil
}

func (ts *templateSet) render(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	var tmpl *template.Template
	if name == "login.html" {
		tmpl = ts.login
	} else {
		tmpl = ts.pages[name]
	}
	if tmpl == nil {
		http.Error(w, "Template not found", http.StatusInternalServerError)
		return
	}
	target := "base.html"
	if name == "login.html" {
		target = "login.html"
	}
	if err := tmpl.ExecuteTemplate(w, target, data); err != nil {
		log.Printf("Template render error (%s): %v", name, err)
	}
}
