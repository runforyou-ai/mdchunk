.PHONY: test fuzz lint golden tidy

test:
	go test -race -count=1 ./...

fuzz:
	./scripts/fuzz.sh 60s

lint:
	go vet ./...
	golangci-lint run ./...

golden:
	go test ./split -run TestGolden -update

tidy:
	go mod tidy
