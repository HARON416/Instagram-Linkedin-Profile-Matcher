package main

import "math/rand/v2"

// Filter and deduplicate before sampling so duplicate spreadsheet rows cannot
// reduce a 20-entry test run to fewer than 20 unique links.
func sampleProfiles(profiles []Profile, username string, limit int) []Profile {
	var eligible []Profile
	for _, profile := range profiles {
		if username == "" || profile.Username == username {
			eligible = append(eligible, profile)
		}
	}
	selected := uniqueProfiles(eligible)
	rand.Shuffle(len(selected), func(i, j int) {
		selected[i], selected[j] = selected[j], selected[i]
	})
	if limit > 0 && len(selected) > limit {
		selected = selected[:limit]
	}
	return selected
}
