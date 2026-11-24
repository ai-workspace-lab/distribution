FROM golang:1.26-alpine AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags='-s -w' -o /out/distd ./cmd/distd

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=builder /out/distd /usr/local/bin/distd
COPY catalog /etc/distd/catalog
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/distd"]
CMD ["serve", "--listen", ":8080", "--config", "/etc/distd/catalog/products.yaml,/etc/distd/catalog/mirrors.yaml,/etc/distd/catalog/upstreams.yaml", "--data-dir", "/var/lib/distd"]
