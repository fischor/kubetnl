FROM golang:alpine AS builder
# Git is required for fetching the dependencies.
RUN apk update && apk add --no-cache git
WORKDIR $GOPATH/src/mypackage/myapp/
COPY . .
RUN GOOS=linux GOARCH=amd64 go build -ldflags="-w -s" -o /dew-server cmd/server/main.go
RUN echo "Dew container running. Waiting for connections." >> /dew.log

FROM alpine
COPY --from=builder /dew-server /usr/local/bin/dew-server
COPY --from=builder /dew.log /var/log/dew.log
ENTRYPOINT tail -f /var/log/dew.log
