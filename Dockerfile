# One image with the controller (gpulifecycle) and the lab node agent (node-agent).
FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal
ARG VERSION=dev
ENV CGO_ENABLED=0
RUN go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/gpulifecycle ./cmd/gpulifecycle \
 && go build -trimpath -ldflags "-s -w" -o /out/node-agent ./cmd/node-agent

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/ /usr/local/bin/
USER 65532:65532
