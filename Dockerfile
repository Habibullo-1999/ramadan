# syntax=docker/dockerfile:1

FROM golang:1.22-alpine AS builder

WORKDIR /app
COPY . .

RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/ramadan-bot ./main.go

FROM alpine:3.20

RUN apk add --no-cache ca-certificates tzdata ttf-dejavu
RUN mkdir -p /data

ENV TZ=Asia/Dushanbe
ENV STATE_FILE=/data/state.json

COPY --from=builder /out/ramadan-bot /usr/local/bin/ramadan-bot

ENTRYPOINT ["/usr/local/bin/ramadan-bot"]
