.PHONY: build test release clean

build:
	go build -o llm-proxy .

test:
	go test ./...

release:
	./scripts/build-release.sh $(VERSION)

clean:
	rm -rf llm-proxy dist/
