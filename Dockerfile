FROM golang:1.26-alpine AS builder
WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download
COPY main.go strikes.go log.go ./
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o autoheal-go .

FROM scratch
COPY --from=builder /build/autoheal-go /autoheal-go
ENTRYPOINT ["/autoheal-go"]
