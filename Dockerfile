# Build example-eventsocket-client.
FROM golang:1.18-alpine as build
RUN apk --no-cache add git
COPY . /go/src/github.com/NEU-SNS/revtr-sidecar
WORKDIR /go/src/github.com/NEU-SNS/revtr-sidecar
RUN go get -v .  && \
    go install . 

# Put it in its own image.
FROM alpine:3.16
COPY --from=build /go/bin/revtr-sidecar /revtr-sidecar
WORKDIR /
ENTRYPOINT ["/revtr-sidecar"]
