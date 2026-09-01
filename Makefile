.PHONY: all build test test-unit test-integration cover cover-html clean

all: build

build:
	go build -trimpath -o autoheal-go .

# --- tests ---------------------------------------------------------------
# test-unit needs nothing but Go. test-integration needs docker, and runs
# everything locally: its own registry, its own images, its own notification
# receiver. Nothing upstream is contacted, so it works offline.
#
# Only test-integration passes -count=1. Go caches a passing result and
# replays it when nothing it can see has changed, which is right for pure code
# and wrong for these: they depend on a daemon, on images and on containers,
# none of which the cache knows about, so a cached pass would say the world was
# fine when only the source was unchanged.

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

# Unit coverage only. The integration suite runs autoheal as a container, and
# collecting coverage from it means building with -cover and mounting a
# GOCOVERDIR for it to write to -- worth doing, not done yet, and a number that
# silently covered only half of what it claimed would be worse than no number.
# The root package only. test/... is the harness, the patient, the receiver and
# the sandbox: things that exist to test autoheal rather than things autoheal
# is made of. Counting them drags the total to a number that says nothing --
# 12% where the code under test is 44% -- because they have no unit tests and
# are not meant to.
cover:
	go test -cover -coverprofile=coverage.out .
	@echo
	@go tool cover -func=coverage.out | tail -1

cover-html: cover
	go tool cover -html=coverage.out

clean:
	rm -f autoheal-go coverage.out
