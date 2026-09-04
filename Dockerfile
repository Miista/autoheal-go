FROM golang:1.26-alpine AS builder
WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download
# Every .go in the package root, rather than a list of filenames: naming them
# individually meant adding a file to the package broke the image build, and
# broke it at compile time inside docker rather than anywhere obvious.
COPY *.go ./
# COVER=1 builds a coverage-instrumented binary, so the integration suite --
# which exercises autoheal as a real container against a real daemon -- can
# report coverage that unit tests structurally cannot reach
# (go.dev/blog/integration-test-coverage). Run it with GOCOVERDIR pointed at a
# mounted directory. Unset, this is an ordinary build.
ARG COVER=0
RUN if [ "$COVER" = "1" ]; then \
      CGO_ENABLED=0 GOOS=linux go build -cover -coverpkg=./... -trimpath -o autoheal-go .; \
    else \
      CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o autoheal-go .; \
    fi

FROM alpine:3 AS certs
RUN apk add --no-cache ca-certificates

FROM scratch
COPY --from=certs /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=builder /build/autoheal-go /autoheal-go
ENTRYPOINT ["/autoheal-go"]
