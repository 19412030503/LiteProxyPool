FROM golang:1.24-alpine AS builder

WORKDIR /src

ARG GOPROXY=https://proxy.golang.org,direct
ENV GOPROXY=${GOPROXY}

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath -ldflags="-s -w" -o /out/lite-proxy .


FROM alpine:3.20

RUN apk add --no-cache ca-certificates tzdata \
  && adduser -D -H -s /sbin/nologin liteproxy

WORKDIR /app

COPY --from=builder /out/lite-proxy /app/lite-proxy
COPY config.docker.json /app/config.json

USER liteproxy

EXPOSE 1080 18080 8088

ENTRYPOINT ["/app/lite-proxy"]
CMD ["-config", "/app/config.json"]
