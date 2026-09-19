FROM golang:1.26 AS builder
ARG TARGETOS
ARG TARGETARCH

WORKDIR /workspace
COPY go.mod go.sum ./
RUN go mod download

COPY Makefile ./
COPY cmd/ cmd/
COPY api/ api/
COPY internal/ internal/

# zz_generated.deepcopy.go is not committed, so it has to be produced here.
# Without it the API types do not satisfy runtime.Object and the build fails.
RUN make generate

RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
    go build -a -ldflags="-s -w" -o manager ./cmd

FROM gcr.io/distroless/static:nonroot
WORKDIR /
COPY --from=builder /workspace/manager .
USER 65532:65532

ENTRYPOINT ["/manager"]
