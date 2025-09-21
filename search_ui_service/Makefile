PROJECTNAME=$(shell basename "$(PWD)")
GO=go

BINARY=search-ui

.PHONY: tidy
tidy:
	$(GO) mod tidy

.PHONY: build
build: tidy
	$(GO) build -o $(BINARY) .

.PHONY: run
run:
	SEARCH_UI_CONFIG_PATH=../deploy/search_ui.config.yaml $(GO) run .

.PHONY: test
test:
	$(GO) test ./...

.PHONY: clean
clean:
	rm -f $(BINARY)