# ebac: the binary is cmd/ebac; nothing is built into the repo root.
.PHONY: build vet test live

build:
	go build -o bin/ebac ./cmd/ebac

vet:
	go vet ./...

test: vet
	go test ./...

# Talks to the real figaro daemon: mints and removes throwaway forms.
# TestLiveRoleSendReachesTheHolder spends a real aria turn, so it is skipped here.
live:
	EBAC_LIVE=1 go test -count=1 -run TestLive -skip TestLiveRoleSendReachesTheHolder ./...
