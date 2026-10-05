package main

import (
	"regexp"
	"testing"
)

// Every locale must define exactly the keys of en.json.
func TestLocalesComplete(t *testing.T) {
	en, err := loadLocale("en")
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range listLocales() {
		m, err := loadLocale(l.Code)
		if err != nil {
			t.Errorf("%s: %v", l.Code, err)
			continue
		}
		for k := range en {
			if _, ok := m[k]; !ok {
				t.Errorf("%s: missing key %q", l.Code, k)
			}
		}
		for k := range m {
			if _, ok := en[k]; !ok {
				t.Errorf("%s: key %q not in en.json", l.Code, k)
			}
		}
	}
	if ls := listLocales(); len(ls) == 0 || ls[0].Code != "en" {
		t.Errorf("English must be listed first: %v", ls)
	}
}

// Every key the page uses must exist in en.json.
func TestIndexKeysExist(t *testing.T) {
	en, _ := loadLocale("en")
	re := regexp.MustCompile(`data-i18n(?:-title)?="([^"]+)"|\bt\('([^']+)'`)
	for _, m := range re.FindAllStringSubmatch(indexHTML, -1) {
		key := m[1] + m[2]
		if _, ok := en[key]; !ok {
			t.Errorf("index.html uses unknown key %q", key)
		}
	}
}
