FROM docker.io/library/golang:1.26 AS builder
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /shackleton ./cmd/shackleton

FROM scratch
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=builder /shackleton /shackleton
ENTRYPOINT ["/shackleton"]
CMD ["serve", "-config=/etc/shackleton/shackleton.yaml"]
