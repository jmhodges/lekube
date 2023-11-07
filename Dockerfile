# Copyright 2021 Jeffrey M Hodges.
# SPDX-License-Identifier: Apache-2.0

# When a new debian stable comes out, we have to update this and the distroless
# base image at the same time. Dependabot won't do it for us, owing to how it
# interprets tags as release histories not versions.
FROM golang:1.21.3-bookworm@sha256:5bafbbb109f02aaf6b41ddc19f54919773c3006f1cbda1599112603367642f0e as build

WORKDIR /go/src/github.com/jmhodges/lekube
ADD . /go/src/github.com/jmhodges/lekube

RUN go build -o /go/bin/lekube

# Now copy it into our base image.
FROM gcr.io/distroless/base-debian12
COPY --from=build /go/bin/lekube /
CMD ["/lekube", "-conf", "/etc/lekube/lekube.json"]
