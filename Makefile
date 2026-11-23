.DEFAULT_GOAL := help

GO ?= go
INPUT ?= instagram_profile_links.xlsx
RESULTS ?= results
LIMIT ?= 5
BINARY ?= bin/profile-matcher
PROFILES ?= 1
CONCURRENCY ?= $(PROFILES)
DELAY_MIN ?= 2s
DELAY_MAX ?= 5s
INSTAGRAM_DELAY_MIN ?= $(DELAY_MIN)
INSTAGRAM_DELAY_MAX ?= $(DELAY_MAX)
LINKEDIN_DELAY_MIN ?= 30s
LINKEDIN_DELAY_MAX ?= 60s
BROWSER_ARGS ?=

.PHONY: help run build fmt vet test check

help:
	@printf '%s\n' \
		'make run      Read XLSX and distribute links across persistent browser profiles' \
		'make build    Build the executable in bin/profile-matcher' \
		'make fmt      Format Go source files' \
		'make vet      Check for common Go mistakes' \
		'make test     Run Go tests' \
		'make check    Run vet and tests' \
		'' \
		'Options: INPUT=workbook.xlsx LIMIT=5 (random sample; 0 means all) RESULTS=results BINARY=bin/profile-matcher' \
		'Browser options: PROFILES=1 BROWSER_ARGS="-limit 5"' \
		'Instagram delays per browser: INSTAGRAM_DELAY_MIN=2s INSTAGRAM_DELAY_MAX=5s' \
		'LinkedIn delays per browser: LINKEDIN_DELAY_MIN=30s LINKEDIN_DELAY_MAX=60s' \
		'Legacy aliases: DELAY_MIN and DELAY_MAX set Instagram delays unless the new names are supplied' \
		'Jev matching: export OPENROUTER_API_KEY; use BROWSER_ARGS="-match=false" to disable' \
		'Match options: BROWSER_ARGS="-match-threshold 85 -match-margin 10 -matches-file results/my-matches.xlsx"'

run:
	$(GO) run . -input "$(INPUT)" -limit "$(LIMIT)" -browser-profiles "$(CONCURRENCY)" -results-dir "$(RESULTS)" -instagram-delay-min "$(INSTAGRAM_DELAY_MIN)" -instagram-delay-max "$(INSTAGRAM_DELAY_MAX)" -linkedin-delay-min "$(LINKEDIN_DELAY_MIN)" -linkedin-delay-max "$(LINKEDIN_DELAY_MAX)" $(BROWSER_ARGS)

build:
	$(GO) build -buildvcs=false -o "$(BINARY)" .

fmt:
	$(GO) fmt ./...

vet:
	$(GO) vet ./...

test:
	$(GO) test ./...

check: vet test
