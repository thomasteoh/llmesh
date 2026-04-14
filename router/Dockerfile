FROM golang:1.25-alpine AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /router ./router/cmd/router/

FROM alpine:3.19
RUN apk add --no-cache ca-certificates
COPY --from=builder /router /router
ENTRYPOINT ["/router"]
