FROM golang:1.27.1-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o sonar ./cmd/sonar

# Lighthouse builds this stage before every deploy: a failure stops the
# deploy. Database tests skip here (no Postgres); scripts/test-db.sh runs them.
FROM builder AS test
RUN apk add --no-cache gcc musl-dev
RUN unformatted="$(gofmt -l .)" && [ -z "$unformatted" ] || { echo "These files need gofmt:"; echo "$unformatted"; exit 1; }
RUN go vet ./...
RUN CGO_ENABLED=1 go test -race -short -count=1 ./...

# Pinned on purpose; bump it deliberately (e.g. for security fixes).
FROM alpine:3.24
# /var/lib/sonar belongs to the agent's user, so a new state volume does too;
# /run/sonar-proxy stays root's: the proxy (root) creates its socket there.
RUN apk add --no-cache ca-certificates \
 && mkdir -p /var/lib/sonar /run/sonar-proxy \
 && chown 10001:10001 /var/lib/sonar
COPY --from=builder /app/sonar /sonar
CMD ["/sonar", "help"]
