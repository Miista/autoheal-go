.PHONY: all build test test-unit test-integration clean

all: build

build:
	go build -trimpath -o autoheal-go .

# --- tests ---------------------------------------------------------------
# test-unit needs nothing but Go. test-integration needs docker, and runs
# everything locally: its own registry, its own images, its own notification
# receiver. Nothing upstream is contacted, so it works offline.
#
# Both pass -count=1. Go caches a passing test's result and replays it when
# nothing it can see has changed, which is right for pure code and misleading
# here: these tests depend on a daemon, on images and on containers, none of
# which the cache knows about. A cached pass would say the world was fine when
# only the source was unchanged.

test: test-unit test-integration

test-unit:
	go test -count=1 -shuffle=on ./...

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
