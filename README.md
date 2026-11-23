# Instagram–LinkedIn Profile Matcher

Find likely LinkedIn profiles for people listed in an Instagram spreadsheet.
The program collects Instagram profile data, searches LinkedIn through TinyFish,
visits candidate profiles, and compares their text using Jev through OpenRouter.
It saves detailed JSON results and an Excel report for review.

## Requirements

- Go 1.25.6 or newer, `make`, and an installed Chrome/Chromium browser.
- TinyFish and OpenRouter API keys.
- An Excel workbook named `instagram_profile_links.xlsx`, with Instagram
  usernames in column A and profile URLs in column B.

## Run

From the project directory, paste the following into your terminal. Replace the
API key placeholders with your own keys:

```sh
export TINYFISH_API_KEY="your-api-key"
export OPENROUTER_API_KEY="your-openrouter-api-key"

make run LIMIT=10 PROFILES=1 INSTAGRAM_DELAY_MIN=1s INSTAGRAM_DELAY_MAX=3s LINKEDIN_DELAY_MIN=60s LINKEDIN_DELAY_MAX=120s
```

This processes **10 randomly selected unique Instagram links** using one browser
session, with 1–3 second pauses between Instagram visits and 60–120 second pauses
before each LinkedIn visit.

Change `LIMIT=10` to `LIMIT=0` in the same command to process **all unique links**:

```sh
make run LIMIT=0 PROFILES=1 INSTAGRAM_DELAY_MIN=1s INSTAGRAM_DELAY_MAX=3s LINKEDIN_DELAY_MIN=60s LINKEDIN_DELAY_MAX=120s
```

After exporting your keys, you can also use these shorter commands with the
program's default delays (2–5 seconds for Instagram and 30–60 seconds for LinkedIn):

```sh
make run LIMIT=10  # Process 10 random unique links
make run LIMIT=0   # Process all unique links
```

Running `make run` without options processes 5 random unique links using one
browser session. Options apply to each command; previous delay settings are not
remembered. Different runs may select overlapping profiles.

If prompted, sign in manually in the browser, then press Enter in the terminal
to continue. Sessions are saved in `.browser-data/` and reused on later runs.
Press Ctrl+C to stop. API keys must be exported in the terminal running the
program; `.env` files are not loaded automatically.

## Results

Results are saved in `results/`:

- **One JSON file per Instagram link:** collected profile data, LinkedIn
  candidates, scores, errors, and the match decision. Processing the same link
  again replaces its JSON file.
- **A new `matches-<UTC timestamp>.xlsx` report per run:** `username`,
  `instagram_url`, `linkedin_urls`, `linkedin_url`, and `score`.

The report lists all search candidates and pairs the highest available score
with its candidate URL, even when the candidate does not qualify as a match.
Check the JSON `match.status` for the decision: `matched`, `no_match`,
`ambiguous`, `skipped`, or `error`.

By default, an accepted match needs a score of at least **85/100** and a lead of
**more than 10 points** over the runner-up. These scores are heuristics, not
verified identity probabilities. Matching compares text, not photos; review the
results before using them.

## Other options

Add these options to your `make run` command as needed:

| Option | Purpose |
| --- | --- |
| `INPUT=profiles.xlsx` | Read another workbook. |
| `PROFILES=4` | Use four independent, persistent browser sessions. |
| `RESULTS=profile-results` | Save results in another directory. |
| `BROWSER_ARGS="-profile roncollins_8"` | Process one Instagram username. |
| `BROWSER_ARGS="-match=false"` | Collect Instagram and LinkedIn data without Jev matching; only TinyFish needs a key. |
| `BROWSER_ARGS="-linkedin-search=false"` | Collect Instagram data only; no API keys needed. |
| `BROWSER_ARGS="-hold=true"` | Keep each captured page open until you close its tab. |

Multiple browser sessions share the queue, with each unique link attempted once
per run. Delays apply per browser. Failed links are recorded without automatic
retries. Login and verification challenges require manual action.

Run `go run . -help` for all CLI flags. For development, use `make build` to create
`bin/profile-matcher` and `make check` to run vet and tests.
