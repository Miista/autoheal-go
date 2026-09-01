.PHONY: all build test test-unit test-integration clean

all: build

build:
	go build -trimpath -o autoheal-go .

# --- tests ---------------------------------------------------------------
# test-unit needs nothing but Go. test-integration needs docker, and runs
# everything locally: its own registry, its own images, its own notification
# receiver. Nothing upstream is contacted, so it works offline.

test: test-unit test-integration

test-unit:
	go test -shuffle=on ./...

# -p 1 keeps packages sequential: the suites share a registry port and a
# testbed.
#
# -shuffle=on because these tests share one testbed directory, one registry and
# one daemon, so a test can pass on what a previous one left behind. Go prints
# the seed, and -shuffle=<seed> replays a failure exactly.
test-integration:
	go test -tags integration -count=1 -p 1 -shuffle=on ./test/...

clean:
	rm -f autoheal-go
