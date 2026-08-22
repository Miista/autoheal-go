FROM golang:1.26-alpine AS builder
WORKDIR /build
COPY go.mod ./
COPY main.go strikes.go ./
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o autoheal-go .

FROM scratch
COPY --from=builder /build/autoheal-go /autoheal-go
ENTRYPOINT ["/autoheal-go"]
