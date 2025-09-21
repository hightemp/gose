PROJECTNAME=$(shell basename "$(PWD)")
GO=go

BINARY=crawler

.PHONY: tidy
tidy:
	$(GO) mod tidy

.PHONY: build
build: tidy
	$(GO) build -o $(BINARY) .

.PHONY: run
run:
	CRAWLER_CONFIG_PATH=../deploy/crawler.config.yaml $(GO) run .

.PHONY: test
test:
	$(GO) test ./...

.PHONY: clean
clean:
	rm -f $(BINARY)