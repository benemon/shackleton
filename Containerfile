# APP_VERSION, not VERSION: the go-toolset base sets ENV VERSION to its Go
# release, which would shadow a same-named build arg.
FROM registry.access.redhat.com/ubi9/go-toolset:latest AS builder
ARG APP_VERSION=dev
USER root
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${APP_VERSION}" -o /shackleton ./cmd/shackleton

FROM registry.access.redhat.com/ubi9/ubi-micro:latest
COPY --from=builder /etc/pki/tls/certs/ca-bundle.crt /etc/pki/tls/certs/ca-bundle.crt
COPY --from=builder /shackleton /shackleton
ENTRYPOINT ["/shackleton"]
CMD ["serve", "-config=/etc/shackleton/shackleton.yaml"]
