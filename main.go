package main

import (
	"archive/zip"
	"context"
	"encoding/xml"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type spreadsheetText struct {
	Text string `xml:"t"`
	Runs []struct {
		Text string `xml:"t"`
	} `xml:"r"`
}

func (s spreadsheetText) String() string {
	var value strings.Builder
	value.WriteString(s.Text)
	for _, run := range s.Runs {
		value.WriteString(run.Text)
	}
	return value.String()
}

var urlPattern = regexp.MustCompile(`https?://[^\s<>"']+`)

type Profile struct {
	Username string
	Link     string
}

// extractProfiles reads usernames from column A and links from column B in
// every worksheet, preserving duplicates and
// the order in which cells occur in the workbook's worksheet files.
func extractProfiles(filename string) ([]Profile, error) {
	workbook, err := zip.OpenReader(filename)
	if err != nil {
		return nil, fmt.Errorf("open workbook: %w", err)
	}
	defer workbook.Close()

	var shared struct {
		Strings []spreadsheetText `xml:"si"`
	}
	for _, file := range workbook.File {
		if file.Name == "xl/sharedStrings.xml" {
			if err := decodeXML(file, &shared); err != nil {
				return nil, err
			}
		}
	}

	var profiles []Profile
	for _, file := range workbook.File {
		if !strings.HasPrefix(file.Name, "xl/worksheets/") ||
			!strings.HasSuffix(file.Name, ".xml") || strings.Contains(file.Name, "/_rels/") {
			continue
		}
		var sheet struct {
			Rows []struct {
				Cells []struct {
					Reference string          `xml:"r,attr"`
					Type      string          `xml:"t,attr"`
					Value     string          `xml:"v"`
					Inline    spreadsheetText `xml:"is"`
				} `xml:"c"`
			} `xml:"sheetData>row"`
		}
		if err := decodeXML(file, &sheet); err != nil {
			return nil, err
		}
		for _, row := range sheet.Rows {
			var username, linkText string
			for _, cell := range row.Cells {
				value := cell.Value
				switch cell.Type {
				case "s":
					index, err := strconv.Atoi(cell.Value)
					if err != nil || index < 0 || index >= len(shared.Strings) {
						return nil, fmt.Errorf("invalid shared string index %q in %s cell %s", cell.Value, file.Name, cell.Reference)
					}
					value = shared.Strings[index].String()
				case "inlineStr":
					value = cell.Inline.String()
				}
				column := strings.TrimRight(cell.Reference, "0123456789")
				switch column {
				case "A":
					username = strings.TrimSpace(value)
				case "B":
					linkText = value
				}
			}
			for _, link := range urlPattern.FindAllString(linkText, -1) {
				profiles = append(profiles, Profile{Username: username, Link: link})
			}
		}
	}
	return profiles, nil
}

func decodeXML(file *zip.File, destination any) error {
	reader, err := file.Open()
	if err != nil {
		return fmt.Errorf("open %s: %w", file.Name, err)
	}
	defer reader.Close()
	if err := xml.NewDecoder(reader).Decode(destination); err != nil {
		return fmt.Errorf("decode %s: %w", file.Name, err)
	}
	return nil
}

func run() error {
	input := flag.String("input", "instagram_profile_links.xlsx", "Excel workbook to read")
	username := flag.String("profile", "", "process only links with this exact Instagram username")
	limit := flag.Int("limit", 5, "number of randomly sampled unique links to process (0 means all)")
	var options runOptions
	flag.StringVar(&options.Chrome, "chrome", "", "Chrome/Chromium executable (auto-detected by default)")
	flag.StringVar(&options.DataDir, "data-dir", ".browser-data", "root for separate persistent browser session directories")
	flag.StringVar(&options.ResultsDir, "results-dir", "results", "directory for captured profile JSON (empty disables capture)")
	flag.BoolVar(&options.LinkedIn, "linkedin-search", true, "search LinkedIn candidates with TinyFish after a successful Instagram capture")
	flag.BoolVar(&options.LinkedInProfiles, "linkedin-profiles", true, "open each LinkedIn candidate and extract the supplied profile section")
	flag.DurationVar(&options.LinkedInTimeout, "linkedin-timeout", 45*time.Second, "page load, lazy scrolling and extraction timeout per LinkedIn candidate")
	flag.DurationVar(&options.LinkedInDelayMin, "linkedin-delay-min", 30*time.Second, "minimum pause before each LinkedIn visit in each browser profile")
	flag.DurationVar(&options.LinkedInDelayMax, "linkedin-delay-max", 60*time.Second, "maximum pause before each LinkedIn visit in each browser profile")
	flag.DurationVar(&options.SearchTimeout, "search-timeout", 30*time.Second, "timeout for a TinyFish search request")
	flag.DurationVar(&options.SearchInterval, "search-interval", 2*time.Second, "minimum interval between TinyFish requests across all browser profiles")
	flag.BoolVar(&options.Matching, "match", true, "score LinkedIn candidates with Jev using profile text and export a matches XLSX")
	flag.StringVar(&options.MatchModel, "match-model", defaultJevModel, "OpenRouter Jev decision model")
	flag.DurationVar(&options.MatchTimeout, "match-timeout", 30*time.Second, "timeout for each Jev decision")
	flag.Float64Var(&options.MatchThreshold, "match-threshold", 85, "minimum Jev score for selecting a candidate (0-100)")
	flag.Float64Var(&options.MatchMargin, "match-margin", 10, "required lead over the next candidate (0-100, strictly greater)")
	flag.StringVar(&options.MatchesFile, "matches-file", "", "new matches XLSX path (default: timestamped file in results-dir)")
	flag.BoolVar(&options.Headless, "headless", false, "run without a browser window (requires -hold=false)")
	flag.BoolVar(&options.Hold, "hold", false, "keep captured pages open until you close their tabs (off by default)")
	flag.IntVar(&options.BrowserProfiles, "browser-profiles", 1, "number of persistent browser profiles sharing the link queue")
	flag.IntVar(&options.BrowserProfiles, "concurrency", 1, "alias for -browser-profiles")
	flag.DurationVar(&options.Timeout, "timeout", 30*time.Second, "timeout for each page to load")
	flag.DurationVar(&options.DelayMin, "instagram-delay-min", 2*time.Second, "minimum pause between Instagram visits in each browser profile")
	flag.DurationVar(&options.DelayMax, "instagram-delay-max", 5*time.Second, "maximum pause between Instagram visits in each browser profile")
	flag.DurationVar(&options.DelayMin, "delay-min", 2*time.Second, "alias for -instagram-delay-min")
	flag.DurationVar(&options.DelayMax, "delay-max", 5*time.Second, "alias for -instagram-delay-max")
	flag.Parse()
	if !options.LinkedIn {
		options.LinkedInProfiles = false
	}
	if !options.LinkedInProfiles {
		options.Matching = false
	}
	if err := options.validate(); err != nil {
		return err
	}
	if *limit < 0 {
		return fmt.Errorf("limit must not be negative")
	}

	profiles, err := extractProfiles(*input)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "Read %d username/link records from %s\n", len(profiles), *input)
	selected := sampleProfiles(profiles, *username, *limit)
	fmt.Fprintf(os.Stderr, "Selected %d randomly sampled unique links for this run\n", len(selected))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := runProfiles(ctx, selected, options); err != nil && ctx.Err() == nil {
		return err
	}
	return nil
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
}
