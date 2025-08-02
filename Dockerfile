FROM golang:1.21 as builder

WORKDIR /app
COPY . .
RUN go build -o rpc-guard main.go

FROM alpine
COPY --from=builder /app/rpc-guard /usr/local/bin/rpc-guard
EXPOSE 18545
CMD ["rpc-guard"]
