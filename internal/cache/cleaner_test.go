package cache

import "testing"

func TestTag(t *testing.T) {
	for cacheType, want := range map[string]string{
		"config":      "CONFIG",
		"block_html":  "BLOCK_HTML",
		"layout":      "LAYOUT_GENERAL_CACHE_TAG",
		"full_page":   "FPC",
		"collections": "COLLECTION_DATA",
		"hyva_svg":    "HYVA_ICONS",
	} {
		if got := Tag(cacheType); got != want {
			t.Errorf("Tag(%q) = %q, want %q", cacheType, got, want)
		}
	}
}
