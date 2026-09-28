FROM golang:1.22-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ENV CGO_ENABLED=0
RUN go build -o /out/router  ./cmd/router \
 && go build -o /out/daemon  ./cmd/daemon \
 && go build -o /out/lemn    ./cmd/lemn \
 && go build -o /out/labeler ./cmd/labeler \
 && go build -o /out/export  ./cmd/export

FROM alpine:3.20
RUN apk add --no-cache ca-certificates curl && adduser -D -u 10001 lemn
COPY --from=build /out/ /usr/local/bin/
WORKDIR /data
RUN chown lemn:lemn /data
USER lemn
