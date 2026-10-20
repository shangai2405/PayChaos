.PHONY: build test demo demo-sdk demo-gif vet validate clean

build:
	go build -o bin/paychaos ./cmd/paychaos

test:
	go test -race -count=1 ./...

vet:
	go vet ./...
	@test -z "$$(gofmt -l .)" || (echo "gofmt needed:"; gofmt -l .; exit 1)

# Checks every bundled scenario file parses and uses known faults.
validate:
	go run ./cmd/paychaos validate examples/scenarios/*.json

demo:
	./scripts/demo.sh

demo-sdk:
	./scripts/demo-sdk.sh

# Regenerates docs/demo.gif from a real run (needs Pillow).
demo-gif:
	./scripts/demo.sh | python3 scripts/render-demo-gif.py docs/demo.gif

clean:
	rm -rf bin paychaos-report*.json paychaos-*.txt paychaos-*.xml paychaos-*.md
