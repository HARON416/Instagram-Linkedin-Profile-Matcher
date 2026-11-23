package main

import (
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/xuri/excelize/v2"
)

type matchExportRow struct {
	Username, InstagramURL, LinkedInURLs, LinkedInURL string
	Score                                             *float64
}

type matchExport struct {
	mu     sync.Mutex
	path   string
	rows   []matchExportRow
	closed bool
}

func newMatchExport(path string) (*matchExport, error) {
	if !strings.EqualFold(filepath.Ext(path), ".xlsx") {
		return nil, errors.New("matches-file must have an .xlsx extension")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	// Reserve the destination exclusively. Later updates replace only this
	// export's own workbook, retaining the last valid ZIP on write failure.
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return nil, fmt.Errorf("create new matches XLSX (choose an unused path): %w", err)
	}
	if err := file.Close(); err != nil {
		return nil, err
	}
	export := &matchExport{path: path}
	if err := export.save(nil); err != nil {
		_ = os.Remove(path)
		return nil, err
	}
	return export, nil
}

func (e *matchExport) Write(profile Profile, match *profileMatch) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return errors.New("matches workbook is closed")
	}
	row := matchExportRow{Username: profile.Username, InstagramURL: profile.Link, LinkedInURL: match.LinkedInURL}
	urls := match.LinkedInURLs
	if len(urls) == 0 {
		for _, candidate := range match.Candidates {
			urls = append(urls, candidate.URL)
		}
	}
	seen := map[string]bool{}
	var unique []string
	for _, url := range urls {
		url = strings.TrimSpace(url)
		if url != "" && !seen[url] {
			unique = append(unique, url)
			seen[url] = true
		}
	}
	row.LinkedInURLs = strings.Join(unique, "\n")
	if match.Score != nil {
		score := *match.Score
		if math.IsNaN(score) || math.IsInf(score, 0) || score < 0 || score > 100 {
			return errors.New("match score must be between 0 and 100")
		}
		row.Score = &score
		// The spreadsheet pairs the score with its candidate even when the
		// detailed JSON status abstains from accepting an identity match.
		if match.BestCandidateURL != "" {
			row.LinkedInURL = match.BestCandidateURL
		} else {
			// Older saved results predate best_candidate_url.
			for _, candidate := range match.Candidates {
				if candidate.Score != nil && *candidate.Score == score {
					row.LinkedInURL = candidate.URL
					break
				}
			}
		}
	}
	next := append(e.rows, row)
	if err := e.save(next); err != nil {
		return err
	}
	e.rows = next
	return nil
}

func (e *matchExport) Close() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.closed = true
	return nil
}

func (e *matchExport) save(rows []matchExportRow) (saveErr error) {
	workbook := excelize.NewFile()
	defer func() { saveErr = errors.Join(saveErr, workbook.Close()) }()
	const sheet = "Matches"
	if err := workbook.SetSheetName("Sheet1", sheet); err != nil {
		return err
	}
	for i, title := range []string{"username", "instagram_url", "linkedin_urls", "linkedin_url", "score"} {
		if err := workbook.SetCellStr(sheet, fmt.Sprintf("%c1", 'A'+i), title); err != nil {
			return err
		}
	}
	headerStyle, err := workbook.NewStyle(&excelize.Style{Font: &excelize.Font{Bold: true}})
	if err != nil {
		return err
	}
	if err := workbook.SetCellStyle(sheet, "A1", "E1", headerStyle); err != nil {
		return err
	}
	textStyle, err := workbook.NewStyle(&excelize.Style{Alignment: &excelize.Alignment{WrapText: true, Vertical: "top"}})
	if err != nil {
		return err
	}
	scoreStyle, err := workbook.NewStyle(&excelize.Style{NumFmt: 2, Alignment: &excelize.Alignment{Vertical: "top"}})
	if err != nil {
		return err
	}
	for i, row := range rows {
		number := i + 2
		lines := 1
		for column, text := range []string{row.Username, row.InstagramURL, row.LinkedInURLs, row.LinkedInURL} {
			// Keep usernames literal, including values beginning with '='.
			if err := workbook.SetCellStr(sheet, fmt.Sprintf("%c%d", 'A'+column, number), text); err != nil {
				return err
			}
			// Leave room for font differences and wrapping at URL punctuation.
			charactersPerLine := 48
			if column == 0 {
				charactersPerLine = 20
			}
			wrappedLines := 0
			for _, line := range strings.Split(text, "\n") {
				wrappedLines += max(1, (utf8.RuneCountInString(line)+charactersPerLine-1)/charactersPerLine)
			}
			lines = max(lines, wrappedLines)
		}
		if err := workbook.SetCellStyle(sheet, fmt.Sprintf("A%d", number), fmt.Sprintf("D%d", number), textStyle); err != nil {
			return err
		}
		// Spreadsheet applications do not consistently auto-fit exported rows.
		// Estimate the height, bounded by Excel's row-height limit.
		if err := workbook.SetRowHeight(sheet, number, math.Min(409, float64(lines)*18+6)); err != nil {
			return err
		}
		if row.Score != nil {
			cell := fmt.Sprintf("E%d", number)
			if err := workbook.SetCellValue(sheet, cell, *row.Score); err != nil {
				return err
			}
			if err := workbook.SetCellStyle(sheet, cell, cell, scoreStyle); err != nil {
				return err
			}
		}
	}
	if err := workbook.SetColWidth(sheet, "A", "A", 25); err != nil {
		return err
	}
	if err := workbook.SetColWidth(sheet, "B", "D", 60); err != nil {
		return err
	}
	if err := workbook.SetColWidth(sheet, "E", "E", 12); err != nil {
		return err
	}
	if err := workbook.SetPanes(sheet, &excelize.Panes{Freeze: true, YSplit: 1, TopLeftCell: "A2", ActivePane: "bottomLeft"}); err != nil {
		return err
	}
	if err := workbook.AutoFilter(sheet, fmt.Sprintf("A1:E%d", len(rows)+1), nil); err != nil {
		return err
	}

	// Write beside the destination, then atomically publish the complete XLSX.
	// A failed write leaves the previous valid workbook intact.
	file, err := os.CreateTemp(filepath.Dir(e.path), ".matches-*.xlsx")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if err := workbook.Write(file); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(file.Name(), e.path)
}
