FROM golang:1.8 as build

WORKDIR /go/src/app
COPY . .

RUN go-wrapper install

# Now copy it into our base image.
FROM gcr.io/distroless/base
COPY --from=build /go/bin/app /
ENTRYPOINT ["/app"]