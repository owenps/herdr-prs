.PHONY: build test run demo demo-popup
build:
	sh scripts/build.sh
test:
	go test -race ./...
run: build
	./bin/herdr-prs
demo: build
	./bin/herdr-prs --demo
demo-popup: build
	herdr plugin link "$(CURDIR)" --enabled
	herdr plugin pane open --plugin herdr-prs --entrypoint demo --focus
