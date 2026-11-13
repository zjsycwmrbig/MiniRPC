FROM golang:1.23-bookworm AS build

ENV GOPROXY=https://goproxy.cn,direct
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /out/echo-server ./cmd/echo-server

FROM alpine:3.20
RUN apk add --no-cache ca-certificates
COPY --from=build /out/echo-server /usr/local/bin/echo-server
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/echo-server"]
