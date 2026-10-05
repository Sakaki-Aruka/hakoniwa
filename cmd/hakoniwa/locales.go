package main

// UI translations. Each locales/<code>.json maps translation keys to text
// ("_name" is the language's own name for the language menu). en.json is the
// reference and the fallback for missing keys. To add a language, add a file
// and rebuild; it appears in the menu automatically.

import (
	"embed"
	"encoding/json"
	"io/fs"
	"net/http"
	"sort"
	"strings"
)

//go:embed locales/*.json
var localeFiles embed.FS

type localeInfo struct {
	Code string `json:"code"`
	Name string `json:"name"`
}

func loadLocale(code string) (map[string]string, error) {
	data, err := localeFiles.ReadFile("locales/" + code + ".json")
	if err != nil {
		return nil, err
	}
	var m map[string]string
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	return m, nil
}

// listLocales returns the embedded languages, English first.
func listLocales() []localeInfo {
	entries, _ := fs.ReadDir(localeFiles, "locales")
	var out []localeInfo
	for _, e := range entries {
		code, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok {
			continue
		}
		m, err := loadLocale(code)
		if err != nil {
			continue
		}
		name := m["_name"]
		if name == "" {
			name = code
		}
		out = append(out, localeInfo{Code: code, Name: name})
	}
	sort.Slice(out, func(i, j int) bool {
		if (out[i].Code == "en") != (out[j].Code == "en") {
			return out[i].Code == "en"
		}
		return out[i].Code < out[j].Code
	})
	return out
}

func registerLocaleRoutes(mux *http.ServeMux) {
	sub, _ := fs.Sub(localeFiles, "locales")
	mux.Handle("GET /locales/", http.StripPrefix("/locales/", http.FileServerFS(sub)))
	locales := listLocales()
	mux.HandleFunc("GET /api/locales", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(locales)
	})
}
