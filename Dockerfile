FROM node:20-bookworm-slim AS web
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ ./
RUN VITE_HOSTED=1 npm run build

FROM golang:1.23-bookworm AS build
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=web /src/web/dist/index.html /src/internal/plugin/web/dist/index.html
RUN CGO_ENABLED=1 GOOS=linux GOARCH="${TARGETARCH}" \
    go build -buildvcs=false -tags cshared -buildmode=c-shared \
    -o /out/cpa-key-policy.so ./cmd/cpa-key-policy
RUN CGO_ENABLED=0 GOOS=linux GOARCH="${TARGETARCH}" \
    go build -buildvcs=false -o /out/cpa-key-policy-migrate ./cmd/cpa-key-policy-migrate

FROM busybox:1.37.0
COPY --from=build /out/cpa-key-policy.so /plugin/cpa-key-policy.so
COPY --from=build /out/cpa-key-policy-migrate /usr/local/bin/cpa-key-policy-migrate
RUN chmod 0755 /plugin/cpa-key-policy.so
