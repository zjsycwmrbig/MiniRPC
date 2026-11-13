FROM golang:1.23-bookworm AS build-base

ENV GOPROXY=https://goproxy.cn,direct
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download
COPY . .

FROM build-base AS server-build
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/echo-server ./cmd/echo-server

FROM build-base AS client-build
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/echo-client ./cmd/echo-client

FROM alpine:3.20 AS client
RUN apk add --no-cache ca-certificates
COPY --from=client-build /out/echo-client /usr/local/bin/echo-client
ENTRYPOINT ["/usr/local/bin/echo-client"]

FROM alpine:3.20 AS server
RUN apk add --no-cache ca-certificates
COPY --from=server-build /out/echo-server /usr/local/bin/echo-server
EXPOSE 8080 9090
ENTRYPOINT ["/usr/local/bin/echo-server"]
