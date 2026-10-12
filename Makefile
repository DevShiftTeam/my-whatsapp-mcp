GO ?= go
GOFMT ?= gofmt
PYTHON ?= python3
TAGS := fts5,sqlite_fts5,ts_omit_logtail
ARGS ?=

.PHONY: build run demo test vet formatcheck script-test ui-test

build:
	GO="$(GO)" sh scripts/app/build.sh

run: build
	./bin/wa-indexer serve $(ARGS)

demo: build
	./bin/wa-indexer demo $(ARGS)

test:
	CGO_ENABLED=1 $(GO) test -tags $(TAGS) ./...

vet:
	CGO_ENABLED=1 $(GO) vet -tags $(TAGS) ./...

formatcheck:
	@unformatted=$$("$(GOFMT)" -l cmd internal) || exit $$?; \
	if [ -n "$$unformatted" ]; then printf '%s\n' "$$unformatted"; exit 1; fi

script-test:
	$(PYTHON) -m unittest discover -s scripts/app -p 'test_*.py'
	$(PYTHON) -m unittest discover -s scripts/package -p 'test_*.py'
	$(PYTHON) -m unittest discover -s services/transcription/tests -p 'test_*.py'

ui-test:
	node --check internal/localui/assets/selects.js
	node --check internal/localui/assets/theme.js
	node --check internal/localui/assets/app.js
	node scripts/ui/test_theme.mjs
	node scripts/ui/test_selects.mjs
	node scripts/ui/test_stats.mjs
	node scripts/ui/test_send_permissions.mjs
