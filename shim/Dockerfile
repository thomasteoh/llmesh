FROM golang:1.26-alpine AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux go build \
    -ldflags="-s -w -X main.version=${VERSION}" \
    -o /shim \
    ./shim/cmd/shim/

FROM alpine:3.19
RUN apk add --no-cache ca-certificates
COPY --from=builder /shim /shim
ENTRYPOINT ["/shim"]
CMD ["-config", "/config.yaml"]
