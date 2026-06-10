FROM golang:1.25-alpine AS builder

ENV GONOSUMDB=* GOPROXY=https://proxy.golang.org,direct GOFLAGS=-mod=mod

RUN apk add --no-cache git

WORKDIR /app
COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o wa-service .

FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata
WORKDIR /app
COPY --from=builder /app/wa-service .

VOLUME ["/data"]
EXPOSE 3000

CMD ["./wa-service"]
