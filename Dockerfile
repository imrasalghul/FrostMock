# Builds FrostMock as a Linux .so. Go plugins only load into a
# Bifrost process built with the same Go version, same OS, same architecture.
#
# Override the toolchain to match your Bifrost build:
#   docker build --build-arg GO_VERSION=1.27.0 -t frostmock .
#   docker create --name t frostmock && docker cp t:/out/chatgpt.so build/ && docker rm t

ARG GO_VERSION=1.27.0
FROM golang:${GO_VERSION} AS build
ARG BUILD_FLAGS=

# buildmode=plugin requires cgo, so gcc must be present (it is, in the golang image).
ENV CGO_ENABLED=1
WORKDIR /src

# Cache dependencies first.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN go build ${BUILD_FLAGS} -buildmode=plugin -o /out/chatgpt.so .
RUN go build ${BUILD_FLAGS} -o /out/chatgpt-login .

FROM scratch AS artifact
COPY --from=build /out/chatgpt.so /out/chatgpt.so
COPY --from=build /out/chatgpt-login /out/chatgpt-login
