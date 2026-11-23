package main

import (
	"fmt"
	"reflect"
	"testing"
)

func TestSampleProfilesUsesUniqueEntriesWithoutChangingSource(t *testing.T) {
	var profiles []Profile
	for i := range 50 {
		profile := Profile{Username: fmt.Sprintf("user%d", i), Link: fmt.Sprintf("https://example.com/%d", i)}
		profiles = append(profiles, profile, profile)
	}
	original := append([]Profile(nil), profiles...)
	selected := sampleProfiles(profiles, "", 20)
	if len(selected) != 20 {
		t.Fatalf("got %d selected links, want 20", len(selected))
	}
	seen := make(map[string]bool)
	for _, profile := range selected {
		if seen[profile.Link] {
			t.Fatalf("repeated selected link: %s", profile.Link)
		}
		seen[profile.Link] = true
		found := false
		for _, source := range original {
			found = found || source == profile
		}
		if !found {
			t.Fatalf("sample corrupted the username/link pair: %+v", profile)
		}
	}
	if !reflect.DeepEqual(profiles, original) {
		t.Fatal("sampling changed the original spreadsheet records")
	}
}

func TestSampleProfilesFiltersAndHandlesSmallDatasets(t *testing.T) {
	first := Profile{Username: "first", Link: "https://example.com/first"}
	second := Profile{Username: "second", Link: "https://example.com/second"}
	profiles := []Profile{first, first, second}
	if got := sampleProfiles(profiles, "first", 20); !reflect.DeepEqual(got, []Profile{first}) {
		t.Fatalf("username filter was not applied before sampling: %v", got)
	}
	if got := sampleProfiles(profiles, "", 20); len(got) != 2 {
		t.Fatalf("small dataset returned %d links, want 2", len(got))
	}
	if got := sampleProfiles(profiles, "", 0); len(got) != 2 {
		t.Fatalf("unlimited sample returned %d links, want 2", len(got))
	}
	if got := sampleProfiles(nil, "", 20); len(got) != 0 {
		t.Fatal("empty workbook selection is not empty")
	}
}

func TestSampleActualWorkbook(t *testing.T) {
	profiles, err := extractProfiles("instagram_profile_links.xlsx")
	if err != nil {
		t.Fatal(err)
	}
	selected := sampleProfiles(profiles, "", 20)
	if len(selected) != 20 || len(uniqueProfiles(selected)) != 20 {
		t.Fatalf("actual workbook did not produce 20 unique links: %d", len(selected))
	}
}
