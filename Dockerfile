FROM node:22-bookworm-slim AS ui
WORKDIR /src/web
COPY web/package*.json ./
RUN npm ci
COPY web/ ./
RUN npm run build

FROM golang:1.26-bookworm AS backend
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
COPY manifests ./manifests
COPY --from=ui /src/web/dist ./web/dist
RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /cncm .

FROM gcr.io/distroless/static-debian12:nonroot
LABEL org.opencontainers.image.licenses="AGPL-3.0-only" \
      org.opencontainers.image.source="https://github.com/tellmeY18/cncm"
COPY LICENSE /usr/share/licenses/cncm/LICENSE
COPY --from=backend /cncm /cncm
ENV CNCM_ADDR=0.0.0.0:8080 CNCM_DATA_DIR=/data
EXPOSE 8080
ENTRYPOINT ["/cncm"]
